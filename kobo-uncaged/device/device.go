package device

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bamiaux/rez"
	"github.com/gofrs/uuid"
	"github.com/kapmahc/epub"
	"github.com/shermp/Kobo-UNCaGED/kobo-uncaged/kuprint"
	"github.com/shermp/Kobo-UNCaGED/kobo-uncaged/util"
)

const koboDBpath = ".kobo/KoboReader.sqlite"
const koboVersPath = ".kobo/version"
const calibreMDfile = "metadata.calibre"
const calibreDIfile = "driveinfo.calibre"
const kuUpdatedMDfile = "metadata_update.kobouc"

const onboardPrefix cidPrefix = "file:///mnt/onboard/"
const sdPrefix cidPrefix = "file:///mnt/sd/"

func newUncagedPassword(passwordList []string) *uncagedPassword {
	return &uncagedPassword{passwordList: passwordList}
}

func (pw *uncagedPassword) NextPassword() string {
	var password string
	if pw.currPassIndex < len(pw.passwordList) {
		password = pw.passwordList[pw.currPassIndex]
		pw.currPassIndex++
	}
	return password
}

// We use a constructor, because nested maps
func CreateKoboMetadata() KoboMetadata {
	var md KoboMetadata
	md.UserMetadata = make(map[string]interface{}, 0)
	md.UserCategories = make(map[string]interface{}, 0)
	md.AuthorSortMap = make(map[string]string, 0)
	md.AuthorLinkMap = make(map[string]string, 0)
	md.Identifiers = make(map[string]string, 0)
	return md
}

// New creates a Kobo object, ready for use
func New(dbRootDir, sdRootDir string, updatingMD bool, opts *KuOptions) (*Kobo, error) {
	var err error
	k := &Kobo{}
	k.Wg = &sync.WaitGroup{}
	k.DBRootDir = dbRootDir
	k.BKRootDir = dbRootDir
	k.ContentIDprefix = onboardPrefix
	fntPath := filepath.Join(k.DBRootDir, ".adds/kobo-uncaged/fonts/LiberationSans-Regular.ttf")
	if k.Kup, err = kuprint.NewPrinter(fntPath); err != nil {
		return nil, err
	}
	k.KuConfig = opts
	if sdRootDir != "" && k.KuConfig.PreferSDCard {
		k.useSDCard = true
		k.BKRootDir = sdRootDir
		k.ContentIDprefix = sdPrefix
	}

	k.Passwords = newUncagedPassword(k.KuConfig.PasswordList)
	headerStr := "Kobo-UNCaGED"
	if k.useSDCard {
		headerStr += "\nUsing SD Card"
	} else {
		headerStr += "\nUsing Internal Storage"
	}

	k.Kup.Println(kuprint.Header, headerStr)
	k.Kup.Println(kuprint.Body, "Gathering information about your Kobo")
	k.InvalidCharsRegex, err = regexp.Compile(`[\\?%\*:;\|\"\'><\$!]`)
	if err != nil {
		return nil, err
	}
	log.Println("Opening NickelDB")
	if err := k.openNickelDB(); err != nil {
		return nil, err
	}
	log.Println("Getting Kobo Info")
	if err := k.getKoboInfo(); err != nil {
		return nil, err
	}
	log.Println("Getting Device Info")
	if err := k.loadDeviceInfo(); err != nil {
		return nil, err
	}
	log.Println("Reading Metadata")
	if err := k.readMDfile(); err != nil {
		return nil, err
	}

	if !updatingMD {
		return k, nil
	}
	if err := k.readUpdateMDfile(); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *Kobo) openNickelDB() error {
	var err error
	dsn := "file:" + filepath.Join(k.DBRootDir, koboDBpath) + "?cache=shared&mode=rw"
	k.nickelDB, err = sql.Open("sqlite3", dsn)
	return err
}

func (k *Kobo) UpdateIfExists(cID string, len int) error {
	if _, exists := k.MetadataMap[cID]; exists {
		var currSize int
		// Make really sure this is in the Nickel DB
		// The query helpfully comes from Calibre
		testQuery := `
			SELECT ___FileSize 
			FROM content 
			WHERE ContentID = ? 
			AND ContentType = 6`
		err := k.nickelDB.QueryRow(testQuery, cID).Scan(&currSize)
		if err != nil {
			return err
		}
		if currSize != len {
			updateQuery := `
				UPDATE content 
				SET ___FileSize = ? 
				WHERE ContentId = ? 
				AND ContentType = 6`
			_, err = k.nickelDB.Exec(updateQuery, len, cID)
			if err != nil {
				return err
			}
			log.Println("Updated existing book file length")
		}
	}
	return nil
}

func (k *Kobo) getKoboInfo() error {
	// Get the model ID and firmware version from the device
	versInfo, err := ioutil.ReadFile(filepath.Join(k.DBRootDir, koboVersPath))
	if err != nil {
		return err
	}
	if len(versInfo) > 0 {
		vers := strings.TrimSpace(string(versInfo))
		versFields := strings.Split(vers, ",")
		fwStr := strings.Split(versFields[2], ".")
		for i, f := range fwStr {
			k.fw[i], _ = strconv.Atoi(f)
		}
		k.Device = koboDevice(versFields[len(versFields)-1])
	}
	return nil
}

func (k *Kobo) GetDeviceOptions() (ext []string, model string, thumbSz image.Point) {
	if k.KuConfig.PreferKepub {
		ext = []string{"kepub", "epub", "mobi", "pdf", "cbz", "cbr", "txt", "html", "rtf"}
	} else {
		ext = []string{"epub", "kepub", "mobi", "pdf", "cbz", "cbr", "txt", "html", "rtf"}
	}
	model = k.Device.Model()
	switch k.KuConfig.Thumbnail.GenerateLevel {
	case generateAll:
		thumbSz = fullCover.Size(k.Device)
	case generatePartial:
		thumbSz = libFull.Size(k.Device)
	default:
		thumbSz = libGrid.Size(k.Device)
	}

	return ext, model, thumbSz
}

// readEpubMeta opens an epub (or kepub), and attempts to read the
// metadata it contains. This is used if the metadata has not yet
// been cached
func (k *Kobo) readEpubMeta(contentID string, md *KoboMetadata) error {
	lpath := util.ContentIDtoLpath(contentID, string(k.ContentIDprefix))
	epubPath := util.ContentIDtoBkPath(k.BKRootDir, contentID, string(k.ContentIDprefix))
	bk, err := epub.Open(epubPath)
	if err != nil {
		return err
	}
	defer bk.Close()
	md.Lpath = lpath
	// Try to get the book identifiers. Note, we prefer the Calibre
	// generated UUID, if available.
	for _, ident := range bk.Opf.Metadata.Identifier {
		switch strings.ToLower(ident.Scheme) {
		case "calibre":
			md.UUID = ident.Data
		case "uuid":
			if md.UUID == "" {
				md.UUID = ident.Data
			}
		default:
			md.Identifiers[ident.Scheme] = ident.Data
		}
	}
	if len(bk.Opf.Metadata.Title) > 0 {
		md.Title = bk.Opf.Metadata.Title[0]
	}
	if len(bk.Opf.Metadata.Description) > 0 {
		desc := html.UnescapeString(bk.Opf.Metadata.Description[0])
		md.Comments = &desc
	}
	if len(bk.Opf.Metadata.Language) > 0 {
		md.Languages = append(md.Languages, bk.Opf.Metadata.Language...)
	}
	for _, author := range bk.Opf.Metadata.Creator {
		if author.Role == "aut" {
			md.Authors = append(md.Authors, author.Data)
		}
	}
	if len(bk.Opf.Metadata.Publisher) > 0 {
		pub := bk.Opf.Metadata.Publisher[0]
		md.Publisher = &pub
	}
	if len(bk.Opf.Metadata.Date) > 0 {
		pubDate := bk.Opf.Metadata.Date[0].Data
		md.Pubdate = &pubDate
	}
	for _, m := range bk.Opf.Metadata.Meta {
		switch m.Name {
		case "calibre:timestamp":
			timeStamp := m.Content
			md.Timestamp = &timeStamp
		case "calibre:series":
			series := m.Content
			md.Series = &series
		case "calibre:series_index":
			seriesIndex, _ := strconv.ParseFloat(m.Content, 64)
			md.SeriesIndex = &seriesIndex
		case "calibre:title_sort":
			md.TitleSort = m.Content
		case "calibre:author_link_map":
			var alm map[string]string
			_ = json.Unmarshal([]byte(html.UnescapeString(m.Content)), &alm)
		}

	}
	return nil
}

// readMDfile loads cached metadata from the "metadata.calibre" JSON file
// and unmarshals (eventially) to a map of KoboMetadata structs, converting
// "lpath" to Kobo's "ContentID", and using that as the map keys
func (k *Kobo) readMDfile() error {
	log.Println("Reading metadata.calibre")

	var koboMD []KoboMetadata
	emptyOrNotExist, err := util.ReadJSON(filepath.Join(k.BKRootDir, calibreMDfile), &koboMD)
	if emptyOrNotExist {
		// ignore
	} else if err != nil {
		return err
	}

	// Make the metadatamap here instead of the constructer so we can pre-allocate
	// the memory with the right size.
	k.MetadataMap = make(map[string]KoboMetadata, len(koboMD))
	// make a temporary map for easy searching later
	tmpMap := make(map[string]int, len(koboMD))
	for n, md := range koboMD {
		contentID := util.LpathToContentID(util.LpathKepubConvert(md.Lpath), string(k.ContentIDprefix))
		tmpMap[contentID] = n
	}
	log.Println("Gathering metadata")
	//spew.Dump(k.MetadataMap)
	// Now that we have our map, we need to check for any books in the DB not in our
	// metadata cache, or books that are in our cache but not in the DB
	var (
		dbCID         string
		dbTitle       *string
		dbAttr        *string
		dbDesc        *string
		dbPublisher   *string
		dbSeries      *string
		dbbSeriesNum  *string
		dbContentType int
		dbMimeType    string
	)
	query := fmt.Sprintf(`
		SELECT ContentID, Title, Attribution, Description, Publisher, Series, SeriesNumber, ContentType, MimeType
		FROM content
		WHERE ContentType=6
		AND MimeType NOT LIKE 'image%%'
		AND (IsDownloaded='true' OR IsDownloaded=1)
		AND ___FileSize>0
		AND Accessibility=-1
		AND ContentID LIKE '%s%%';`, k.ContentIDprefix)

	bkRows, err := k.nickelDB.Query(query)
	if err != nil {
		return err
	}
	defer bkRows.Close()
	for bkRows.Next() {
		err = bkRows.Scan(&dbCID, &dbTitle, &dbAttr, &dbDesc, &dbPublisher, &dbSeries, &dbbSeriesNum, &dbContentType, &dbMimeType)
		if err != nil {
			return err
		}
		if _, exists := tmpMap[dbCID]; !exists {
			log.Printf("Book not in cache: %s\n", dbCID)
			bkMD := CreateKoboMetadata()
			bkMD.Comments, bkMD.Publisher, bkMD.Series = dbDesc, dbPublisher, dbSeries
			if dbTitle != nil {
				bkMD.Title = *dbTitle
			}
			if dbbSeriesNum != nil {
				index, err := strconv.ParseFloat(*dbbSeriesNum, 64)
				if err == nil {
					bkMD.SeriesIndex = &index
				}
			}
			if dbAttr != nil {
				bkMD.Authors = strings.Split(*dbAttr, ",")
				for i := range bkMD.Authors {
					bkMD.Authors[i] = strings.TrimSpace(bkMD.Authors[i])
				}
			}
			if dbMimeType == "application/epub+zip" || dbMimeType == "application/x-kobo-epub+zip" {
				err = k.readEpubMeta(dbCID, &bkMD)
				if err != nil {
					log.Print(err)
				}
			}
			fi, err := os.Stat(filepath.Join(k.BKRootDir, bkMD.Lpath))
			if err == nil {
				bkSz := fi.Size()
				lastMod := fi.ModTime().Format(time.RFC3339)
				bkMD.LastModified = &lastMod
				bkMD.Size = int(bkSz)
			}
			//spew.Dump(bkMD)
			k.MetadataMap[dbCID] = bkMD
		} else {
			k.MetadataMap[dbCID] = koboMD[tmpMap[dbCID]]
		}
	}
	err = bkRows.Err()
	if err != nil {
		return err
	}
	// Hopefully, our metadata is now up to date. Update the cache on disk
	err = k.WriteMDfile()
	if err != nil {
		return err
	}
	return nil
}

func (k *Kobo) WriteMDfile() error {
	var n int
	metadata := make([]KoboMetadata, len(k.MetadataMap))
	for _, md := range k.MetadataMap {
		metadata[n] = md
		n++
	}
	return util.WriteJSON(filepath.Join(k.BKRootDir, calibreMDfile), metadata)
}

func (k *Kobo) readUpdateMDfile() error {
	emptyOrNotExist, err := util.ReadJSON(filepath.Join(k.BKRootDir, kuUpdatedMDfile), &k.UpdatedMetadata)
	if emptyOrNotExist {
		// ignore
	} else if err != nil {
		log.Println(err)
		return err
	}
	return nil
}

func (k *Kobo) WriteUpdateMDfile() error {
	// We only write the file if there is new or updated metadata to write
	if len(k.UpdatedMetadata) == 0 {
		return nil
	}
	return util.WriteJSON(filepath.Join(k.BKRootDir, kuUpdatedMDfile), k.UpdatedMetadata)
}

func (k *Kobo) loadDeviceInfo() error {
	emptyOrNotExist, err := util.ReadJSON(filepath.Join(k.BKRootDir, calibreDIfile), &k.DriveInfo.DevInfo)
	if emptyOrNotExist {
		uuid4, _ := uuid.NewV4()
		k.DriveInfo.DevInfo.LocationCode = "main"
		k.DriveInfo.DevInfo.DeviceName = "Kobo " + k.Device.Model()
		k.DriveInfo.DevInfo.DeviceStoreUUID = uuid4.String()
		if k.useSDCard {
			k.DriveInfo.DevInfo.LocationCode = "A"
		}
	} else if err != nil {
		return err
	}
	return nil
}

func (k *Kobo) SaveDeviceInfo() error {
	return util.WriteJSON(filepath.Join(k.BKRootDir, calibreDIfile), k.DriveInfo.DevInfo)
}

func (k *Kobo) SaveCoverImage(contentID string, size image.Point, imgB64 string) {
	defer k.Wg.Done()

	img, _, err := image.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(imgB64)))
	if err != nil {
		log.Println(err)
		return
	}
	sz := img.Bounds().Size()

	imgDir := ".kobo-images"
	if k.useSDCard {
		imgDir = "koboExtStorage/images-cache"
	}
	imgDir = filepath.Join(k.BKRootDir, imgDir)
	imgID := util.ImgIDFromContentID(contentID)
	jpegOpts := jpeg.Options{Quality: k.KuConfig.Thumbnail.JpegQuality}

	var coverEndings []koboCover
	switch k.KuConfig.Thumbnail.GenerateLevel {
	case generateAll:
		coverEndings = []koboCover{fullCover, libFull, libGrid}
	case generatePartial:
		coverEndings = []koboCover{libFull, libGrid}
	}
	for _, cover := range coverEndings {
		nsz := cover.Resize(k.Device, sz)
		nfn := filepath.Join(imgDir, cover.RelPath(imgID))

		log.Printf("Resizing %s cover to %s (target %s) for %s\n", sz, nsz, cover.Size(k.Device), cover)

		var nimg image.Image
		if !sz.Eq(nsz) {
			nimg = image.NewYCbCr(image.Rect(0, 0, nsz.X, nsz.Y), img.(*image.YCbCr).SubsampleRatio)
			rez.Convert(nimg, img, k.KuConfig.Thumbnail.rezFilter)
			log.Printf(" -- Resized to %s\n", nimg.Bounds().Size())
		} else {
			nimg = img
			log.Println(" -- Skipped resize: already correct size")
		}
		// Optimization. No need to resize libGrid from the full cover size...
		if cover == libFull {
			img = nimg
		}

		if err := os.MkdirAll(filepath.Dir(nfn), 0755); err != nil {
			log.Println(err)
			continue
		}

		lf, err := os.OpenFile(nfn, os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			log.Println(err)
			continue
		}

		if err := jpeg.Encode(lf, nimg, &jpegOpts); err != nil {
			log.Println(err)
			lf.Close()
		}
		lf.Close()
	}
}

// updateNickelDB updates the Nickel database with updated metadata obtained from a previous run
func (k *Kobo) UpdateNickelDB() error {
	// No matter what happens, we remove the 'metadata_update.kobouc' file when we're done
	defer os.Remove(filepath.Join(k.BKRootDir, kuUpdatedMDfile))
	query := `
		UPDATE content SET 
		Description=?,
		Series=?,
		SeriesNumber=?,
		SeriesNumberFloat=? 
		WHERE ContentID=?`
	stmt, err := k.nickelDB.Prepare(query)
	if err != nil {
		return err
	}
	var desc, series, seriesNum *string
	var seriesNumFloat *float64
	for _, cid := range k.UpdatedMetadata {
		desc, series, seriesNum, seriesNumFloat = nil, nil, nil, nil
		if k.MetadataMap[cid].Comments != nil && *k.MetadataMap[cid].Comments != "" {
			desc = k.MetadataMap[cid].Comments
		}
		if k.MetadataMap[cid].Series != nil && *k.MetadataMap[cid].Series != "" {
			series = k.MetadataMap[cid].Series
		}
		if k.MetadataMap[cid].SeriesIndex != nil && *k.MetadataMap[cid].SeriesIndex != 0.0 {
			sn := strconv.FormatFloat(*k.MetadataMap[cid].SeriesIndex, 'f', -1, 64)
			seriesNum = &sn
			seriesNumFloat = k.MetadataMap[cid].SeriesIndex
		}
		_, err = stmt.Exec(desc, series, seriesNum, seriesNumFloat, cid)
		if err != nil {
			log.Print(err)
		}
	}
	return nil
}

func (k *Kobo) Close() {
	k.Kup.Println(kuprint.Body, "Waiting for thumbnail generation to complete")
	k.Wg.Wait()
	k.Kup.Close()
	k.nickelDB.Close()

}

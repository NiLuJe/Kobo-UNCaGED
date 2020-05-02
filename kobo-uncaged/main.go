// Copyright 2019-2020 Sherman Perry

// This file is part of Kobo UNCaGED.

// Kobo UNCaGED is free software: you can redistribute it and/or modify
// it under the terms of the Affero GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Kobo UNCaGED is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with Kobo UNCaGED.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pelletier/go-toml"
	"github.com/shermp/Kobo-UNCaGED/kobo-uncaged/device"
	"github.com/shermp/Kobo-UNCaGED/kobo-uncaged/kunc"
	"github.com/shermp/UNCaGED/uc"
)

type returnCode int

// Note, this is set by the go linker at build time
var kuVersion string

const (
	genericError    returnCode = 250
	successNoAction returnCode = 0
	successRerun    returnCode = 1
	successUSBMS    returnCode = 10
	passwordError   returnCode = 100
	calibreNotFound returnCode = 101
)

func getUserOptions(dbRootDir string) (*device.KuOptions, error) {
	// Note, we return opts, regardless of whether we successfully read the options file.
	// Our code can handle the default struct gracefully
	opts := &device.KuOptions{}
	configBytes, err := ioutil.ReadFile(filepath.Join(dbRootDir, ".adds/kobo-uncaged/config/ku.toml"))
	if err != nil {
		return opts, fmt.Errorf("error loading config file: %w", err)
	}
	if err := toml.Unmarshal(configBytes, opts); err != nil {
		return opts, fmt.Errorf("error reading config file: %w", err)
	}
	opts.Thumbnail.Validate()
	opts.Thumbnail.SetRezFilter()
	return opts, nil
}

func saveUserOptions(dbRootDir string, opts *device.KuOptions) error {
	configBytes, err := toml.Marshal(opts)
	if err != nil {
		return fmt.Errorf("error marshaling config: %w", err)
	}
	if err = ioutil.WriteFile(filepath.Join(dbRootDir, ".adds/kobo-uncaged/config/ku.toml"), configBytes, 0644); err != nil {
		return fmt.Errorf("error writing config file: %w", err)
	}
	return nil
}

func returncodeFromError(err error, k *device.Kobo) returnCode {
	rc := successNoAction
	if err != nil {
		log.Print(err)
		if k == nil {
			return genericError
		}
		var calErr uc.CalError
		if errors.As(err, &calErr) {
			switch calErr {
			case uc.CalibreNotFound:
				k.MsgChan <- device.WebMsg{Body: "Calibre not found!<br>Have you enabled the Calibre Wireless service?", Progress: -1}
				//kuprint.Println(kuprint.Body, "Calibre not found!\nHave you enabled the Calibre Wireless service?")
				rc = calibreNotFound
			case uc.NoPassword:
				k.MsgChan <- device.WebMsg{Body: "No valid password found!", Progress: -1}
				//kuprint.Println(kuprint.Body, "No valid password found!")
				rc = passwordError
			default:
				k.MsgChan <- device.WebMsg{Body: calErr.Error(), Progress: -1}
				//kuprint.Println(kuprint.Body, calErr.Error())
				rc = genericError
			}
		}
		k.MsgChan <- device.WebMsg{Body: err.Error(), Progress: -1}
		//kuprint.Println(kuprint.Body, err.Error())
		rc = genericError
	}
	return rc
}
func mainWithErrCode() returnCode {
	w, err := syslog.New(syslog.LOG_DEBUG, "KoboUNCaGED")
	if err == nil {
		log.SetOutput(w)
	}
	onboardMntPtr := flag.String("onboardmount", "/mnt/onboard", "If changed, specify the new new mountpoint of '/mnt/onboard'")
	sdMntPtr := flag.String("sdmount", "", "If changed, specify the new new mountpoint of '/mnt/sd'")
	bindAddrPtr := flag.String("bindaddr", "127.0.0.1:80", "Specify the network address and port <IP:POrt> to listen on")
	//mdPtr := flag.Bool("metadata", false, "Updates the Kobo DB with new metadata")
	flag.Parse()
	//fntPath := filepath.Join(*onboardMntPtr, ".adds/kobo-uncaged/fonts/LiberationSans-Regular.ttf")
	// if err = kuprint.InitPrinter(fntPath); err != nil {
	// 	log.Print(err)
	// 	return genericError
	// }
	// defer kuprint.Close()
	log.Println("Started Kobo-UNCaGED")
	log.Println("Reading options")
	opts, optErr := getUserOptions(*onboardMntPtr)
	log.Println("Creating KU object")
	k, err := device.New(*onboardMntPtr, *sdMntPtr, *bindAddrPtr, opts, kuVersion)
	if err != nil {
		log.Print(err)
		return returncodeFromError(err, nil)
	}
	defer k.Close()
	if optErr != nil {
		k.MsgChan <- device.WebMsg{Body: optErr.Error(), Progress: -1}
		//kuprint.Println(kuprint.Body, optErr.Error())
	}
	// if *mdPtr {
	// 	log.Println("Updating Metadata")
	// 	k.MsgChan <- device.WebMsg{Body: "Updating Metadata!", Progress: -1}
	// 	//kuprint.Println(kuprint.Body, "Updating Metadata!")
	// 	_, err = k.UpdateNickelDB()
	// 	if err != nil {
	// 		log.Print(err)
	// 		return returncodeFromError(err, k)
	// 	}
	// 	k.MsgChan <- device.WebMsg{Body: "Metadata Updated!<br>Returning to Home screen", Progress: -1}
	// 	//kuprint.Println(kuprint.Body, "Metadata Updated!\n\nReturning to Home screen")
	// } else {
	log.Println("Preparing Kobo UNCaGED!")
	ku := kunc.New(k)
	cc, err := uc.New(ku, k.KuConfig.EnableDebug)
	if err != nil {
		log.Print(err)
		return returncodeFromError(err, k)
	}
	log.Println("Starting Calibre Connection")
	err = cc.Start()
	if err != nil {
		log.Print(err)
		return returncodeFromError(err, k)
	}

	if len(k.UpdatedMetadata) > 0 {
		rerun, err := k.UpdateNickelDB()
		if err != nil {
			k.MsgChan <- device.WebMsg{Body: "Updating metadata failed", Progress: -1}
			//kuprint.Println(kuprint.Body, "Updating metadata failed")
			log.Print(err)
			return returncodeFromError(err, k)
		}
		if rerun {
			if k.KuConfig.AddMetadataByTrigger {
				k.MsgChan <- device.WebMsg{Body: "Books added!<br>Your Kobo will perform another USB connect after content import", Progress: -1}
				//kuprint.Println(kuprint.Body, "Books added!\n\nYour Kobo will perform another USB connect after content import")
				return successUSBMS
			}
			k.MsgChan <- device.WebMsg{Body: "Books added!<br>Kobo-UNCaGED will restart automatically to update metadata", Progress: -1}
			//kuprint.Println(kuprint.Body, "Books added!\n\nKobo-UNCaGED will restart automatically to update metadata")
			return successRerun
		}
	}
	k.MsgChan <- device.WebMsg{Body: "Nothing more to do!<br>Returning to Home screen", Progress: -1}
	//kuprint.Println(kuprint.Body, "Nothing more to do!\n\nReturning to Home screen")
	//}

	return successNoAction
}
func main() {
	os.Exit(int(mainWithErrCode()))
}

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
	"log"
	"log/syslog"
	"os"

	_ "github.com/mattn/go-sqlite3"
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

				rc = calibreNotFound
			case uc.NoPassword:
				k.MsgChan <- device.WebMsg{Body: "No valid password found!", Progress: -1}

				rc = passwordError
			default:
				k.MsgChan <- device.WebMsg{Body: calErr.Error(), Progress: -1}

				rc = genericError
			}
		}
		k.MsgChan <- device.WebMsg{Body: err.Error(), Progress: -1}

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
	bindAddrPtr := flag.String("bindaddr", "127.0.0.1:8181", "Specify the network address and port <IP:POrt> to listen on")

	flag.Parse()
	log.Println("Started Kobo-UNCaGED")
	log.Println("Reading options")
	log.Println("Creating KU object")
	k, err := device.New(*onboardMntPtr, *sdMntPtr, *bindAddrPtr, kuVersion)
	if err != nil {
		log.Print(err)
		return returncodeFromError(err, nil)
	}
	defer k.Close()

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

			log.Print(err)
			return returncodeFromError(err, k)
		}
		if rerun {
			if k.KuConfig.AddMetadataByTrigger {
				k.MsgChan <- device.WebMsg{Body: "Books added!<br>Your Kobo will perform another USB connect after content import", Progress: -1}

				return successUSBMS
			}
			k.MsgChan <- device.WebMsg{Body: "Books added!<br>Kobo-UNCaGED will restart automatically to update metadata", Progress: -1}

			return successRerun
		}
	}
	k.MsgChan <- device.WebMsg{Body: "Nothing more to do!<br>Returning to Home screen", Progress: -1}

	return successNoAction
}
func main() {
	os.Exit(int(mainWithErrCode()))
}

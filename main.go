package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"path"
	"sync"
	"time"
)

// Settings

var (
	RemoteFolderName = flag.String("putio-folder", "Putio Desktop", "putio folder name under your root")
	AccessToken      = flag.String("oauth-token", "", "Oauth Token")
	LocalFolderPath  = flag.String("local-path", "~/Putio Desktop", "local folder to fetch")
	CheckInterval    = flag.Int("check-minutes", 5, "check interval of remote files in put.io")
)

// Globals

var (
	RemoteFolderId  int
	TotalDownloaded int64
	TotalToDownload int64
	TotalFilesSize  int64
)

// Download functions

func WalkAndDownload(parentId int, folderPath string, runWg *sync.WaitGroup, reportCh chan Report) {
	defer runWg.Done()
	log.Println("Walking in:", folderPath)

	// Creating if the folder is absent
	if _, err := os.Stat(folderPath); err != nil {
		err := os.Mkdir(folderPath, 0755)
		if err != nil {
			// Will try again at the next run
			log.Println(err)
			return
		}
	}

	files, err := FilesListRequest(parentId)
	if err != nil {
		log.Println(err)
		return
	}

	for _, file := range files {
		path := path.Join(folderPath, file.Name)
		if file.ContentType == "application/x-directory" {
			runWg.Add(1)
			go WalkAndDownload(file.Id, path, runWg, reportCh)
		} else {
			reportCh <- Report{FilesSize: file.Size}
			if _, err := os.Stat(path); err != nil {
				runWg.Add(1)
				go DownloadFile(file, path, runWg, reportCh)
			}
		}
	}
}

func StartWalkAndDownloadClearReports(RemoteFolderId int, reportCh chan Report) {
	TotalFilesSize = 0
	TotalDownloaded = 0
	TotalToDownload = 0
	var runWg sync.WaitGroup
	runWg.Add(1)
	go WalkAndDownload(RemoteFolderId, *LocalFolderPath, &runWg, reportCh)
	runWg.Wait()
}

type Report struct {
	Downloaded int64
	ToDownload int64
	FilesSize  int64
}

func HumanReadableSpeed(bytePerSec float64) string {
	if bytePerSec > 1024*1024 {
		return fmt.Sprintf("%5.2f MB/s", bytePerSec/(1024*1024))
	} else if bytePerSec > 1024 {
		return fmt.Sprintf("%5.1f KB/s", bytePerSec/1024)
	} else {
		return fmt.Sprintf("%5.0f B/s ", bytePerSec)
	}
}

func Reporter(reportCh chan Report) {
	lastRecordedTime := time.Now()
	lastRecordedTotalDownloaded := int64(0)
	minReportTime := 1 * time.Second
	log.Println("Reporter started")

	for report := range reportCh {
		TotalDownloaded += report.Downloaded
		TotalToDownload += report.ToDownload
		TotalFilesSize += report.FilesSize
		currentTime := time.Now()
		lastReportTimeDifference := currentTime.Sub(lastRecordedTime)
		if lastReportTimeDifference > minReportTime {
			remainingDownload := TotalToDownload - TotalDownloaded
			syncPercentage := 100 - (float32(remainingDownload) / float32(TotalFilesSize) * 100)
			completePercentage := float32(TotalDownloaded) / float32(TotalToDownload) * 100
			speed := (float64(TotalDownloaded) - float64(lastRecordedTotalDownloaded)) / lastReportTimeDifference.Seconds()
			fmt.Printf("[ Downloads %% %2.0f - %s ]   [ Sync: %% %5.2f ]\r", completePercentage, HumanReadableSpeed(speed), syncPercentage)
			lastRecordedTime = currentTime
			lastRecordedTotalDownloaded = TotalDownloaded
		}

	}
}

func StartReporter() chan Report {
	reportCh := make(chan Report)
	go Reporter(reportCh)
	return reportCh
}

func main() {
	log.Println("Starting...")
	flag.Parse()

	RemoteFolderId, err := GetRemoteFolderId()
	if err != nil {
		log.Fatal(err)
	}

	// If local folder path is left at default value, find os users home directory
	// and name "Putio Folder" as the local folder path under it
	if *LocalFolderPath == "~/Putio Desktop" {
		user, _ := user.Current()
		defaultPath := path.Join(user.HomeDir, "Putio Desktop")
		LocalFolderPath = &defaultPath
	}

	reportCh := StartReporter()

	for {
		log.Println("Started syncing...")
		StartWalkAndDownloadClearReports(RemoteFolderId, reportCh)
		time.Sleep(time.Duration(*CheckInterval) * time.Minute)
	}

	log.Println("Exiting...")
}

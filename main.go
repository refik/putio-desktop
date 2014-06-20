package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path"
	"strconv"
	"sync"
	"time"
)

// Settings

var RemoteFolderName = flag.String("putio-folder", "Putio Desktop", "putio folder name under your root")
var AccessToken = flag.String("oauth-token", "", "Oauth Token")
var LocalFolderPath = flag.String("local-path", "~/Putio Desktop", "local folder to fetch")
var CheckInterval = flag.Int("check-minutes", 5, "check interval of remote files in put.io")

const ApiUrl = "https://api.put.io/v2/"
const DownloadExtension = ".ptdownload"
const MaxConnection = 5
const ChunkSize int64 = 32 * 1024

// Globals

var RemoteFolderId int

// Putio api response types

type FilesResponse struct {
	Files []File `json:"files"`
}

type FileResponse struct {
	File File `json:"file"`
}

type File struct {
	Id          int    `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

func (file *File) DownloadUrl() string {
	method := "files/" + strconv.Itoa(file.Id) + "/download"
	return MakeUrl(method, map[string]string{})
}

// Api request functions

func ParamsWithAuth(params map[string]string) string {
	newParams := url.Values{}

	for k, v := range params {
		newParams.Add(k, v)
	}

	newParams.Add("oauth_token", *AccessToken)
	return newParams.Encode()
}

func MakeUrl(method string, params map[string]string) string {
	newParams := ParamsWithAuth(params)
	return ApiUrl + method + "?" + newParams
}

func GetRemoteFolderId() (folderId int, err error) {
	// Making sure this folder exits. Creating if necessary
	// and updating global variable
	files, err := FilesListRequest(0)
	if err != nil {
		return
	}

	// Looping through files to get the putitin folder
	for _, file := range files {
		if file.Name == *RemoteFolderName {
			log.Println("Found remote folder")
			return file.Id, nil
		}
	}

	postUrl := MakeUrl("files/create-folder", map[string]string{})
	postValues := url.Values{}
	postValues.Add("name", *RemoteFolderName)
	postValues.Add("parent_id", "0")
	resp, err := http.PostForm(postUrl, postValues)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	fileResponse := FileResponse{}
	err = json.Unmarshal(body, &fileResponse)
	if err != nil {
		return
	}

	return fileResponse.File.Id, nil
}

func FilesListRequest(parentId int) (files []File, err error) {
	// Preparing url
	params := map[string]string{"parent_id": strconv.Itoa(parentId)}
	folderUrl := MakeUrl("files/list", params)

	resp, err := http.Get(folderUrl)
	if err != nil {
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	filesResponse := FilesResponse{}
	err = json.Unmarshal(body, &filesResponse)
	if err != nil {
		return
	}

	return filesResponse.Files, nil
}

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

// Downloads the given range. In case of an error, sleeps for 10s and tries again.
func DownloadRange(file *File, fp *os.File, offset int64, size int64, rangeWg *sync.WaitGroup, chunkIndex bitField, reportCh chan Report) {
	defer rangeWg.Done()
	reportCh <- Report{ToDownload: size}
	newOffset := offset
	lastByte := offset + size           // The byte we won't be getting
	lastIndex := lastByte/ChunkSize - 1 // The last index we'll fill

	// Creating a custom request because it will have Range header in it
	req, _ := http.NewRequest("GET", file.DownloadUrl(), nil)

	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, lastByte-1)
	req.Header.Add("Range", rangeHeader)

	// http.DefaultClient does not copy headers while following redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Add("Range", rangeHeader)
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return
	}
	defer resp.Body.Close()

	buffer := make([]byte, ChunkSize)
	for {
		nr, er := io.ReadFull(resp.Body, buffer)
		if nr > 0 {
			nw, ew := fp.WriteAt(buffer[0:nr], newOffset)
			nWritten := int64(nw)
			newOffset += nWritten
			currentIndex := newOffset/ChunkSize - 1
			if currentIndex == lastIndex && newOffset != lastByte {
				// dont mark the last bit done without finishing the whole range
			} else {
				chunkIndex.Set(currentIndex)
				fp.WriteAt(chunkIndex, file.Size)
			}
			reportCh <- Report{Downloaded: nWritten}
			if ew != nil {
				log.Println(ew)
				return
			}
		}
		if er == io.EOF || er == io.ErrUnexpectedEOF {
			return
		}
		if er != nil {
			log.Println(er)
			return
		}
	}
}

func DownloadFile(file File, path string, runWg *sync.WaitGroup, reportCh chan Report) error {
	defer runWg.Done()
	downloadPath := path + DownloadExtension
	chunkIndex := bitField(make([]byte, file.Size/ChunkSize/8+1))
	resume := false
	var fp *os.File

	// Creating a waitgroup to wait for all chunks to be
	// downloaded before returning
	var rangeWg sync.WaitGroup

	// Checking whether previous download exists
	if _, err := os.Stat(downloadPath); err != nil {
		log.Println("Downloading:", file.Name)
		fp, err = os.Create(downloadPath)
		if err != nil {
			log.Println(err)
			return err
		}
		defer fp.Close()

		// Allocating space for the file
		err = FillWithZeros(fp, file.Size+int64(len(chunkIndex)))
		if err != nil {
			log.Println(err)
			return err
		}
	} else {
		log.Println("Resuming:", file.Name)
		resume = true
		fp, err = os.OpenFile(downloadPath, os.O_RDWR, 0755)
		if err != nil {
			log.Println(err)
			return err
		}
		defer fp.Close()
		_, err = fp.ReadAt(chunkIndex, file.Size)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	rangeSize := file.Size / MaxConnection
	excessBytes := file.Size % MaxConnection

	offset := int64(0)
	for i := 0; i < MaxConnection; i++ {
		rangeCustomOffset := offset
		offset += rangeSize
		rangeCustomSize := rangeSize
		if i == MaxConnection-1 {
			// Add excess bytes to last connection
			rangeCustomSize = rangeSize + excessBytes
		}
		if resume {
			// Adjusting range for previously downloaded file
			startIndex := rangeCustomOffset / ChunkSize
			limitIndex := (rangeCustomOffset + rangeSize) / ChunkSize

			zIndex, err := chunkIndex.GetFirstZeroIndex(startIndex, limitIndex)
			if err == nil {
				// This range in not finished yet
				zByteIndex := zIndex * ChunkSize
				rangeCustomSize -= zByteIndex - rangeCustomOffset
				rangeCustomOffset = zByteIndex
			} else {
				continue
			}
		}
		rangeWg.Add(1)
		go DownloadRange(&file, fp, rangeCustomOffset, rangeCustomSize, &rangeWg, chunkIndex, reportCh)
	}

	// Waiting for all chunks to be downloaded
	rangeWg.Wait()

	// Verifying the download, some ranges may not be finished
	_, err := chunkIndex.GetFirstZeroIndex(0, file.Size/ChunkSize)
	if err == nil {
		// All chunks are not downloaded
		log.Println("All chunks are not downloaded, closing file for dowload:", file.Name)
		fp.Close()
		return nil
	}

	// Renaming the file to correct path
	fp.Truncate(file.Size)
	fp.Close()
	err = os.Rename(downloadPath, path)
	if err != nil {
		log.Println(err)
		return err
	}

	log.Println("Download completed:", file.Name)
	return nil
}

// Utility functions

func FillWithZeros(fp *os.File, remainingWrite int64) error {
	var nWrite int64 // Next chunk size to write
	zeros := make([]byte, ChunkSize)
	for remainingWrite > 0 {
		// Checking whether there is less to write than chunkSize
		if remainingWrite < ChunkSize {
			nWrite = remainingWrite
		} else {
			nWrite = ChunkSize
		}

		_, err := fp.Write(zeros[0:nWrite])
		if err != nil {
			return err
		}

		remainingWrite -= nWrite
	}
	return nil
}

type bitField []byte

// Set bit i. 0 is the most significant bit.
func (b bitField) Set(i int64) {
	div, mod := divMod(i, 8)
	b[div] |= 1 << (7 - uint32(mod))
}

// Test bit i. 0 is the most significant bit.
func (b bitField) Test(i int64) bool {
	div, mod := divMod(i, 8)
	return (b[div] & (1 << (7 - uint32(mod)))) > 0
}

func (b bitField) GetFirstZeroIndex(offset, limit int64) (int64, error) {
	for pos := offset; pos < limit; pos++ {
		if bit := b.Test(pos); bit == false {
			return pos, nil
		}
	}
	return 0, errors.New("No zero in given range")
}

func divMod(a, b int64) (int64, int64) { return a / b, a % b }

func StartWalkAndDownloadClearReports(RemoteFolderId int, reportCh chan Report) {
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

func Reporter(reportCh chan Report) {
	log.Println("Reporter started")

	var (
		totalDownloaded int64
		totalToDownload int64
		totalFilesSize  int64
	)

	for report := range reportCh {
		totalDownloaded += report.Downloaded
		totalToDownload += report.ToDownload
		totalFilesSize += report.FilesSize
		remainingDownload := totalToDownload - totalDownloaded
		syncPercentage := 100 - (float32(remainingDownload) / float32(totalFilesSize) * 100)
		completePercentage := float32(totalDownloaded) / float32(totalToDownload) * 100
		fmt.Printf("Downloads: %% %3.0f  -  Sync: %% %6.2f\r", completePercentage, syncPercentage)
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

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
var LocalFolderPath = flag.String("local-path", "home/Putio Desktop", "local folder to fetch")
var CheckInterval = flag.Int("check-minutes", 10, "check interval of remote files in put.io")

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

func WalkAndDownload(parentId int, folderPath string) {
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
			go WalkAndDownload(file.Id, path)
		} else {
			if _, err := os.Stat(path); err != nil {
				go DownloadFile(file, path)
			}
		}
	}
}

// Downloads the given range. In case of an error, sleeps for 10s and tries again.
func DownloadRange(file *File, fp *os.File, offset int64, size int64, rangeWg *sync.WaitGroup, chunkIndex bitField) {
	defer rangeWg.Done()
	newOffset := offset

	retry := func(err error) {
		log.Println(err, "Retrying in 10s...")
		time.Sleep(10 * time.Second)
		written := newOffset - offset
		rangeWg.Add(1)
		go DownloadRange(file, fp, newOffset, size-written, rangeWg, chunkIndex)
	}

	// Creating a custom request because it will have Range header in it
	req, _ := http.NewRequest("GET", file.DownloadUrl(), nil)

	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+size)
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
		retry(err)
		return
	}
	defer resp.Body.Close()

	buffer := make([]byte, ChunkSize)
	for {
		nr, er := io.ReadFull(resp.Body, buffer)
		if nr > 0 {
			nw, ew := fp.WriteAt(buffer[0:nr], newOffset)
			newOffset += int64(nw)
			chunkIndex.Set(newOffset/ChunkSize - 1)
			fp.WriteAt(chunkIndex, file.Size)
			if ew != nil {
				log.Println(newOffset)
				retry(ew)
				return
			}
		}
		if er == io.EOF || er == io.ErrUnexpectedEOF {
			return
		}
		if er != nil {
			retry(er)
			return
		}
	}
}

func DownloadFile(file File, path string) error {
	downloadPath := path + DownloadExtension
	chunkIndex := bitField(make([]byte, file.Size/ChunkSize+1))
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
	rangeWg.Add(MaxConnection)
	for i := 0; i < MaxConnection; i++ {
		if i == MaxConnection-1 {
			// Add excess bytes to last connection
			rangeSize += excessBytes
		}
		if resume {
			// Adjusting range for previously downloaded file
			startIndex := offset / ChunkSize
			limitIndex := (offset + rangeSize) / ChunkSize

			zIndex, err := chunkIndex.GetFirstZeroIndex(startIndex, limitIndex)
			if err != nil {
				zByteIndex := zIndex * ChunkSize
				previouslyDownloaded := zByteIndex - offset
				remaining := rangeSize - previouslyDownloaded
				newRangeSize := remaining - (remaining % ChunkSize)
				go DownloadRange(&file, fp, zByteIndex, newRangeSize, &rangeWg, chunkIndex)
			}
		} else {

			go DownloadRange(&file, fp, offset, rangeSize, &rangeWg, chunkIndex)
		}
		offset += rangeSize
	}

	// Waiting for all chunks to be downloaded
	rangeWg.Wait()

	// Renaming the file to correct path
	fp.Truncate(file.Size)
	fp.Close()
	err := os.Rename(path+DownloadExtension, path)
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

func main() {
	log.Println("Starting...")
	flag.Parse()

	RemoteFolderId, err := GetRemoteFolderId()
	if err != nil {
		log.Fatal(err)
	}

	// If local folder path is left at default value, find os users home directory
	// and name "Putio Folder" as the local folder path under it
	if *LocalFolderPath == "home/Putio Desktop" {
		user, _ := user.Current()
		defaultPath := path.Join(user.HomeDir, "Putio Desktop")
		LocalFolderPath = &defaultPath
	}

	for {
		go WalkAndDownload(RemoteFolderId, *LocalFolderPath)
		time.Sleep(time.Duration(*CheckInterval) * time.Minute)
	}

	log.Println("Exiting...")
}

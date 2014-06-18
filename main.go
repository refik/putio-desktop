package main

import (
	"encoding/json"
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
const DownloadExtension = ".putio-desktop"
const MaxConnection = 10

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
	Size        int    `json:"size"`
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

func WalkAndDownload(parentId int, folderPath string) {
	log.Println("Walking in", folderPath)

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

// Local file operations

func FillWithZeros(fp *os.File, remainingWrite int) error {
	var nWrite int // Next chunk size to write
	chunkSize := 3 * 1024
	zeros := make([]byte, chunkSize)
	for remainingWrite > 0 {
		// Checking whether there is less to write than chunkSize
		if remainingWrite < chunkSize {
			nWrite = remainingWrite
		} else {
			nWrite = chunkSize
		}

		_, err := fp.Write(zeros[0:nWrite])
		if err != nil {
			return err
		}

		remainingWrite -= nWrite
	}
	return nil
}

func DownloadChunk(file *File, fp *os.File, offset int, size int, chunkWg *sync.WaitGroup, fileLock *sync.Mutex) {
	newOffset := offset

	retry := func(err error) {
		chunkWg.Add(1)
		log.Println(err)
		written := newOffset - offset
		go DownloadChunk(file, fp, newOffset, size-written, chunkWg, fileLock)
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
		log.Println(err)
		go DownloadChunk(file, fp, offset, size, chunkWg, fileLock)
	}
	defer resp.Body.Close()

	buffer := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buffer)
		if nr > 0 {
			// Using lock to not mix up the writes
			fileLock.Lock()
			fp.Seek(int64(offset), os.SEEK_SET)
			nw, ew := fp.Write(buffer[0:nr])
			fileLock.Unlock()
			newOffset += nw
			if ew != nil {
				retry(ew)
				break
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			retry(er)
			break
		}
	}
	chunkWg.Done()
}

func DownloadFile(file File, path string) error {
	log.Println("Downloading:", file.Name)

	// Creating a waitgroup to wait for all chunks to be
	// downloaded before returning
	var chunkWg sync.WaitGroup

	// Creating a lock for file so that writes from different
	// chunk downloaders do not mix
	var fileLock sync.Mutex

	// Creating the file under a temporary name for download
	fp, err := os.Create(path + DownloadExtension)
	if err != nil {
		log.Println(err)
		return err
	}
	defer fp.Close()

	// Allocating space for the file
	err = FillWithZeros(fp, file.Size)
	if err != nil {
		log.Println(err)
		return err
	}

	chunkSize := file.Size / MaxConnection
	excessBytes := file.Size % MaxConnection

	offset := 0
	chunkWg.Add(MaxConnection)
	for i := 0; i < MaxConnection; i++ {
		if i == MaxConnection-1 {
			// Add excess bytes to last connection
			chunkSize += excessBytes
		}
		go DownloadChunk(&file, fp, offset, chunkSize, &chunkWg, &fileLock)
		offset += chunkSize
	}

	// Waiting for all chunks to be downloaded
	chunkWg.Wait()

	// Renaming the file to correct path
	fp.Close()
	err = os.Rename(path+DownloadExtension, path)
	if err != nil {
		log.Println(err)
		return err
	}

	log.Println("Download completed:", file.Name)
	return nil
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

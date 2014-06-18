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
var RemoteFolderId int
var RemoteFolderName = flag.String("putio-folder", "Putio Desktop", "putio folder name under your root")
var AccessToken = flag.String("oauth-token", "", "Oauth Token")
var LocalFolderPath = flag.String("local-path", "home/Putio Desktop", "local folder to fetch")
var CheckInterval = flag.Int("check-minutes", 5, "check interval of remote files in put.io")
var ApiUrl = "https://api.put.io/v2/"

const DownloadExtension = ".putiodl"
const MaxConnection = 10

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
	return MakeUrl(method, &map[string]string{})
}

// Utility functions
func ParamsWithAuth(params *map[string]string) string {
	newParams := url.Values{}

	for k, v := range *params {
		newParams.Add(k, v)
	}

	newParams.Add("oauth_token", *AccessToken)
	return newParams.Encode()
}

func MakeUrl(method string, params *map[string]string) string {
	newParams := ParamsWithAuth(params)
	return ApiUrl + method + "?" + newParams
}

func GetRemoteFolderId() int {
	// Making sure this folder exits. Creating if necessary
	// and updating global variable
	files := FilesListRequest(0)

	// Looping through files to get the putitin folder
	for _, file := range files {
		if file.Name == *RemoteFolderName {
			log.Println("Found remote folder")
			return file.Id
		}
	}

	postUrl := MakeUrl("files/create-folder", &map[string]string{})
	postValues := url.Values{}
	postValues.Add("name", *RemoteFolderName)
	postValues.Add("parent_id", "0")
	resp, err := http.PostForm(postUrl, postValues)
	if err != nil {
		log.Fatal(err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatal(err)
	}
	fileResponse := FileResponse{}
	err = json.Unmarshal(body, &fileResponse)
	if err != nil {
		log.Fatal(err)
	}

	folderId := fileResponse.File.Id
	log.Println("Remote folder ID:", fileResponse.File)
	return folderId
}

func FilesListRequest(parentId int) []File {
	// Preparing url
	params := &map[string]string{"parent_id": strconv.Itoa(parentId)}
	folderUrl := MakeUrl("files/list", params)

	log.Println(folderUrl)
	resp, err := http.Get(folderUrl)
	if err != nil {
		log.Fatal(err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	filesResponse := FilesResponse{}
	json.Unmarshal(body, &filesResponse)

	return filesResponse.Files
}

func WalkAndDownload(parentId int, folderPath string) {
	// Creating if the encapsulating folder is absent
	if _, err := os.Stat(folderPath); err != nil {
		err := os.Mkdir(folderPath, 0755)
		if err != nil {
			log.Fatal(err)
		}
	}

	files := FilesListRequest(parentId)
	log.Println("Walking in", folderPath)

	for _, file := range files {
		path := path.Join(folderPath, file.Name)
		if file.ContentType == "application/x-directory" {
			go WalkAndDownload(file.Id, path)
		} else {
			if _, err := os.Stat(path); err != nil {
				log.Println(err)
				go DownloadFile(&file, path)
			}
		}
	}
}

func DownloadChunk(file *File, fp *os.File, offset int, size int, chunkWg *sync.WaitGroup, fileLock *sync.Mutex) {
	defer chunkWg.Done()
	log.Println("Downloading chunk starting from:", offset, "bytes:", size)

	req, err := http.NewRequest("GET", file.DownloadUrl(), nil)
	if err != nil {
		log.Fatal(err)
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+size)
	req.Header.Add("Range", rangeHeader)
	log.Println(rangeHeader)
	log.Println(req.Header)

	// http.DefaultClient does not copy headers while following redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Add("Range", rangeHeader)
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	buffer := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buffer)
		if nr > 0 {
			fileLock.Lock()
			fp.Seek(int64(offset), os.SEEK_SET)
			nw, ew := fp.Write(buffer[0:nr])
			fileLock.Unlock()
			offset += nw
			if ew != nil {
				log.Fatal(ew)
			}
		}
		if er == io.EOF {
			log.Println("Seen EOF")
			break
		}
		if er != nil {
			log.Println(er)
			break
		}
	}
}

func FillWithZeros(fp *os.File, remainingWrite int) {
	var write int
	chunkSize := 3 * 1024
	zeros := make([]byte, chunkSize)
	for remainingWrite > 0 {
		if remainingWrite < chunkSize {
			write = remainingWrite
		} else {
			write = chunkSize
		}
		fp.Write(zeros[0:write])
		remainingWrite -= write
	}
}

func DownloadFile(file *File, path string) {
	// Creating a waitgroup to wait for all chunks to be
	// downloaded before exiting
	var chunkWg sync.WaitGroup
	var fileLock sync.Mutex

	fp, err := os.Create(path + DownloadExtension)
	if err != nil {
		log.Fatal(err)
	}

	// Allocating space for the file
	FillWithZeros(fp, file.Size)

	chunkSize := file.Size / MaxConnection
	excessBytes := file.Size % MaxConnection

	log.Println("Chunk size:", chunkSize, "Excess:", excessBytes)

	offset := 0
	chunkWg.Add(MaxConnection)
	for i := MaxConnection; i > 0; i-- {
		if i == 1 {
			// Add excess bytes to last connection
			chunkSize += excessBytes
		}
		go DownloadChunk(file, fp, offset, chunkSize, &chunkWg, &fileLock)
		offset += chunkSize
	}
	chunkWg.Wait()

	fp.Close()
	er := os.Rename(path+DownloadExtension, path)
	if er != nil {
		log.Fatal(err)
	}

	log.Println("Download completed")
}

func main() {
	flag.Parse()
	RemoteFolderId := GetRemoteFolderId()
	if *LocalFolderPath == "home/Putio Desktop" {
		user, _ := user.Current()
		defaultPath := path.Join(user.HomeDir, "Putio Desktop")
		LocalFolderPath = &defaultPath
	}
	log.Println("Starting...")

	for {
		go WalkAndDownload(RemoteFolderId, *LocalFolderPath)
		time.Sleep(time.Duration(*CheckInterval) * time.Minute)
	}

	log.Println("Exiting...")
}

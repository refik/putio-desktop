package main

import (
	"os"
    "io"
    "fmt"
	"log"
    "time"
    "flag"
	"path"
    "sync"
    "syscall"
	"strconv"
	"net/url"
	"net/http"
	"io/ioutil"
	"encoding/json"
)

// Settings
var RemoteFolderId int
var RemoteFolderName = flag.String("putio-folder", "",
                                   "putio folder name under your root")
var AccessToken = flag.String("oauth-token", "", "Oauth Token")
var LocalFolderPath = flag.String("local-path", "", "local folder to fetch")
const ApiUrl = "https://api.put.io/v2/"
const DownloadExtension = ".putiodl"
const MaxConnection = 10

// Putio api response types
type FilesResponse struct {
	Files []File `json:"files"`
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

// Utility functions
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

func SaveRemoteFolderId() {
	// Making sure this folder exits. Creating if necessary
	// and updating global variable
	files := FilesListRequest(0)
	log.Println(files)
	// Looping through files to get the putitin folder
	for _, file := range files {
		if file.Name == *RemoteFolderName {
			log.Println("Found putitin folder")
			RemoteFolderId = file.Id
		}
	}
}

func FilesListRequest(parentId int) []File {
	// Preparing url
	params := map[string]string{"parent_id": strconv.Itoa(parentId)}
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
				DownloadFile(&file, path)
			}
		}
	}
}

func DownloadChunk(file *File, fp *os.File, offset int,
                   size int, chunkWg *sync.WaitGroup) {
    defer chunkWg.Done()
    log.Println("Downloading chunk starting from:", offset, "bytes:", size)

    req, err := http.NewRequest("GET", file.DownloadUrl(), nil)
    if err != nil {
        log.Fatal(err)
    }

    rangeHeader := fmt.Sprintf("bytes=%d-%d\r\n", offset, offset + size)
    log.Println(rangeHeader)
    req.Header.Add("Range", rangeHeader)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    buffer := make([]byte, 32*1024)
    for {
        nr, er := resp.Body.Read(buffer)
        if nr > 0 {
            nw, ew := fp.WriteAt(buffer[0:nr], int64(offset))
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

func DownloadFile(file *File, path string) {
    // Creating a waitgroup to wait for all chunks to be 
    // downloaded before exiting
    var chunkWg sync.WaitGroup

    fp, err := os.Create(path + DownloadExtension)
    if err != nil {
        log.Fatal(err)
    }
    defer fp.Close()

    // Allocating space for the file
    syscall.Fallocate(int(fp.Fd()), 0, 0, int64(file.Size))

    chunkSize := file.Size / MaxConnection
    excessBytes := file.Size % MaxConnection

    log.Println("Chunk size:", chunkSize, "Excess:", excessBytes)

    offset := 0
    for i := MaxConnection; i > 0; i-- {
        if i == 1 {
            // Add excess bytes to last connection
            chunkSize += excessBytes
        }
        chunkWg.Add(1)
        go DownloadChunk(file, fp, offset, chunkSize, &chunkWg)
        offset += chunkSize
    }
    chunkWg.Wait()

    fp.Close()
    er := os.Rename(path + DownloadExtension, path)
    if er != nil {
        log.Fatal(err)
    }

    log.Println("Download completed")
}

func init() {
    flag.Parse()
	SaveRemoteFolderId()
}

func main() {
	log.Println("Starting...")

    for {
        WalkAndDownload(RemoteFolderId, *LocalFolderPath)
        time.Sleep(10 * time.Minute)
    }

	log.Println("Exiting...")
}

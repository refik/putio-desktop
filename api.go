package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
)

const ApiUrl = "https://api.put.io/v2/"

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
	return MakeUrl(method, nil)
}

func ParamsWithAuth(params map[string]string) string {
	newParams := url.Values{}

	if params != nil {
		for k, v := range params {
			newParams.Add(k, v)
		}

	}

	newParams.Add("oauth_token", *AccessToken)
	return newParams.Encode()
}

func MakeUrl(method string, params map[string]string) string {
	newParams := ParamsWithAuth(params)
	return ApiUrl + method + "?" + newParams
}

// Makes sure remote folder
func GetRemoteFolderId() (folderId int, err error) {
	// Making sure this folder exits. Creating and updating
	// global variable if necessary
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

	postUrl := MakeUrl("files/create-folder", nil)
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

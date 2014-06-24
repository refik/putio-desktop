package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
)

const DownloadExtension = ".ptdownload"
const ChunkSize int64 = 32 * 1024
const MaxConnection = 10

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
				if zByteIndex > rangeCustomOffset {
					rangeCustomSize -= zByteIndex - rangeCustomOffset
					rangeCustomOffset = zByteIndex
				}

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

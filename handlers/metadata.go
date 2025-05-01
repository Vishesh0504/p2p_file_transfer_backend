package handlers

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"p2p_file_transfer/utils"
	"path/filepath"
	"time"
)
var RootPath = "/home/vishesh"

type FileMetadata struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	IsDir    bool           `json:"isDir"`
	Size     int64          `json:"size,omitempty"`
	Mode     fs.FileMode    `json:"mode,omitempty"`
	ModTime  time.Time      `json:"modTime"`
	Children []FileMetadata `json:"children,omitempty"`
}
func MakeFileMetadata(root *FileMetadata) error {
    path,_ := url.QueryUnescape(root.Path)
    entries, err := os.ReadDir(path)
    if err != nil {
        return fmt.Errorf("reading directory %s: %w", root.Path, err)
    }
    for _, entry := range entries {
        fileInfo, err := entry.Info()
        if err != nil {
            return fmt.Errorf("getting info for %s: %w", entry.Name(), err)
        }
        fileEntry := FileMetadata{
            Name:    entry.Name(),
            Path:    filepath.Join(path, entry.Name()),
            IsDir:   entry.IsDir(),
            Mode:    fileInfo.Mode(),
            ModTime: fileInfo.ModTime(),
        }
        if !entry.IsDir() {
            fileEntry.Size = fileInfo.Size()
        } else {
            // For directories, add their immediate children (one level deep)
            subEntries, subErr := os.ReadDir(fileEntry.Path)
            if subErr == nil {
                for _, subEntry := range subEntries {
                    subFileInfo, subInfoErr := subEntry.Info()
                    if subInfoErr != nil {
                        continue // skip problematic entries
                    }
                    child := FileMetadata{
                        Name:    subEntry.Name(),
                        Path:    filepath.Join(fileEntry.Path, subEntry.Name()),
                        IsDir:   subEntry.IsDir(),
                        Mode:    subFileInfo.Mode(),
                        ModTime: subFileInfo.ModTime(),
                    }
                    if !subEntry.IsDir() {
                        child.Size = subFileInfo.Size()
                    }
                    fileEntry.Children = append(fileEntry.Children, child)
                }
            }
        }
        root.Children = append(root.Children, fileEntry)
    }
    return nil
}

type UserRequest struct{
	Path string `json:"path"`
}

func MetadataHandler(w http.ResponseWriter,r *http.Request){
	path :=RootPath
	var req UserRequest
	decoder:=json.NewDecoder(r.Body)
	err:=decoder.Decode(&req)
	if err!=nil{
		utils.RespondWithError(w,http.StatusBadRequest,err.Error())
	}
	if req.Path != ""{
		path = path+"/"+req.Path
	}
	root := &FileMetadata{
		Name: filepath.Base(path),
		Path: path,
		IsDir: true,
	}
	if err := MakeFileMetadata(root); err != nil {
		log.Printf("Error building file metadata: %v", err)
		utils.RespondWithError(w,http.StatusInternalServerError,"Failed to generate metadata");
	}


	utils.RespondWithJSON(w,http.StatusOK,root)

}
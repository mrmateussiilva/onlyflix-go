package types

import "time"

type SyncVideo struct {
	Id      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

type SyncFolder struct {
	Id     string      `json:"id"`
	Name   string      `json:"name"`
	Videos []SyncVideo `json:"videos"`
}

type SyncCatalog struct {
	LastSync   time.Time    `json:"last_sync"`
	Folders    []SyncFolder `json:"folders"`
	RootVideos []SyncVideo  `json:"root_videos"`
}

// Package widgets is the module of the spectest test app: its request types,
// the functions that answer them and the models they return.
package widgets

import (
	"context"
	"time"
)

// Widget is the model the widget routes return.
type Widget struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Tags      []string  `json:"tags"`
	Note      string    `json:"note,omitempty"`
	Parent    *Widget   `json:"parent"`
	CreatedAt time.Time `json:"created_at"`
}

// AdminWidget is a widget with the fields only an administrator sees.
type AdminWidget struct {
	Widget
	Owner string `json:"owner"`
}

// File is a stored file.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type ListRequest struct {
	Search string `json:"search"`
}

func (r *ListRequest) Validate() error { return nil }

type ListResponse struct {
	Widgets []Widget `json:"widgets"`
}

func List(ctx context.Context, request *ListRequest) (*ListResponse, error) { return nil, nil }

type GetRequest struct {
	ID int64 `json:"id"`
}

func (r *GetRequest) Validate() error { return nil }

func Get(ctx context.Context, request *GetRequest) (*Widget, error) { return nil, nil }

type AdminRequest struct {
	ID int64 `json:"id"`
}

func (r *AdminRequest) Validate() error { return nil }

func GetAdmin(ctx context.Context, request *AdminRequest) (*AdminWidget, error) { return nil, nil }

type CreateRequest struct {
	Name string `json:"name"`
}

func (r *CreateRequest) Validate() error { return nil }

func Create(ctx context.Context, request *CreateRequest) (*Widget, error) { return nil, nil }

type FileRequest struct {
	Path string `json:"path"`
}

func (r *FileRequest) Validate() error { return nil }

func GetFile(ctx context.Context, request *FileRequest) (*File, error) { return nil, nil }

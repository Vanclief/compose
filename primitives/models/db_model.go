package models

type ModelWithCursor interface {
	GetCursor() string
}

// TODO:
// Add insert, update, delete methods to the ModelWithCursor interface

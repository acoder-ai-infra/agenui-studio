package memory

import "github.com/AGenUI/agenui-studio/harness/internal/storage"

func storageInvalid(msg string) error  { return storage.NewError(storage.ErrInvalidArgument, msg) }
func storageNotFound(msg string) error { return storage.NewError(storage.ErrNotFound, msg) }
func storageConflict(msg string) error { return storage.NewError(storage.ErrConflict, msg) }
func storageCAS(msg string) error      { return storage.NewError(storage.ErrCASMismatch, msg) }
func storageIllegal(msg string) error  { return storage.NewError(storage.ErrIllegalTransition, msg) }

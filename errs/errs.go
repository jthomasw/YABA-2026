package errs

import (
	"errors"
)

type BadRequest string

func (err BadRequest) Error() string {
	return string(err)
}

func Cause(err error) error {
	var badRequest BadRequest
	if errors.As(err, &badRequest) {
		return badRequest
	}
	return err
}

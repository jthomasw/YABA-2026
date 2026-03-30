package errs

import "errors"

type BadRequest string

func (err BadRequest) Error() string {
	return string(err)
}

type Unauthorized string

func (err Unauthorized) Error() string {
	return string(err)
}

type NotFound string

func (err NotFound) Error() string {
	return string(err)
}

func Cause(err error) error {
	var badRequest BadRequest
	if errors.As(err, &badRequest) {
		return badRequest
	}
	var unauthorized Unauthorized
	if errors.As(err, &unauthorized) {
		return unauthorized
	}
	var notFound NotFound
	if errors.As(err, &notFound) {
		return notFound
	}
	return err
}

package foo

import (
	"context"
	"fmt"

	"github.com/jthomasw/YABA-2026/errs"
)

type Service struct {
	barRepository BarRepository
	idGenerator   IdGenerator
}

type BarRepository interface {
	GetBarById(ctx context.Context, id string) (Bar, error)
	StoreBar(ctx context.Context, bar Bar) error
}

type IdGenerator interface {
	GenerateId() (string, error)
}

type Bar struct {
	Id string
	B  int
	A  int
	R  int
}

type ServiceAttachments struct {
	BarRepository BarRepository
	IdGenerator   IdGenerator
}

func NewService(attachments ServiceAttachments) *Service {
	return &Service{
		barRepository: attachments.BarRepository,
		idGenerator:   attachments.IdGenerator,
	}
}

func (service *Service) CreateBar(ctx context.Context, request CreateBarRequest) (Bar, error) {
	// we could have another interface for user authentication here
	issuesWithRequest := request.Issues()
	if len(issuesWithRequest) > 0 {
		return Bar{}, errs.BadRequest(fmt.Sprintf("There were one or more issues with the request: %v", issuesWithRequest))
	}
	id, err := service.idGenerator.GenerateId()
	if err != nil {
		return Bar{}, fmt.Errorf("could not generate id: %w", err)
	}
	newBar := Bar{
		Id: id,
		B:  request.B,
		A:  request.A,
		R:  request.R,
	}
	err = service.barRepository.StoreBar(ctx, newBar)
	if err != nil {
		return Bar{}, fmt.Errorf("could not store new bar in repository: %w", err)
	}
	return newBar, nil
}

type CreateBarRequest struct {
	// We could have a field/struct for credentials to authenticate the user
	B int
	A int
	R int
}

func (request *CreateBarRequest) Issues() string {
	issues := ""
	aIsInvalid := request.A < 0
	if aIsInvalid {
		issues = "A cannot be less than 0"
	}
	bIsInvalid := request.B > 10
	if bIsInvalid {
		issues = "B cannot be greater than 10"
	}
	rIsInvalid := request.R%6 == 0
	if rIsInvalid {
		issues = "R cannot be a multiple of 6"
	}
	return issues
}

func (service *Service) GetBarById(ctx context.Context, request GetBarByIdRequest) (Bar, error) {
	// we can authenticate the user here
	bar, err := service.barRepository.GetBarById(ctx, request.Id)
	if err != nil {
		return Bar{}, fmt.Errorf("could not get bar from repository: %w", err)
	}
	return bar, nil
}

type GetBarByIdRequest struct {
	// we could have a credential field/struct here to authenticate the user
	Id string
}

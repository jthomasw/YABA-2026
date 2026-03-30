package user

import (
	"context"
	"fmt"

	"myapp/errs"

	"golang.org/x/crypto/bcrypt"
)

// User is the core domain object.
type User struct {
	ID       int64
	Username string
	Password string // hashed
}

// UserRepository is the interface the sqlite layer must satisfy.
// Business logic depends on this interface, never on the concrete DB type.
type UserRepository interface {
	StoreUser(ctx context.Context, username, hashedPassword string) error
	GetUserByUsername(ctx context.Context, username string) (User, error)
}

// Service holds all user-related business logic.
type Service struct {
	userRepository UserRepository
}

// ServiceAttachments carries the dependencies injected into NewService.
type ServiceAttachments struct {
	UserRepository UserRepository
}

func NewService(attachments ServiceAttachments) *Service {
	return &Service{
		userRepository: attachments.UserRepository,
	}
}

// --- Register ---

type RegisterRequest struct {
	Username string
	Password string
}

func (r *RegisterRequest) Issues() string {
	if r.Username == "" {
		return "username is required"
	}
	if r.Password == "" {
		return "password is required"
	}
	if len(r.Password) < 6 {
		return "password must be at least 6 characters"
	}
	return ""
}

func (s *Service) Register(ctx context.Context, req RegisterRequest) error {
	if issue := req.Issues(); issue != "" {
		return errs.BadRequest(issue)
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("could not hash password: %w", err)
	}

	err = s.userRepository.StoreUser(ctx, req.Username, string(hashedPassword))
	if err != nil {
		return fmt.Errorf("could not store user: %w", err)
	}

	return nil
}

// --- Login ---

type LoginRequest struct {
	Username string
	Password string
}

// Authenticate verifies credentials and returns the username on success.
func (s *Service) Authenticate(ctx context.Context, req LoginRequest) (string, error) {
	if req.Username == "" || req.Password == "" {
		return "", errs.BadRequest("username and password are required")
	}

	u, err := s.userRepository.GetUserByUsername(ctx, req.Username)
	if err != nil {
		return "", fmt.Errorf("could not fetch user: %w", err)
	}

	err = bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(req.Password))
	if err != nil {
		return "", errs.Unauthorized("invalid username or password")
	}

	return u.Username, nil
}

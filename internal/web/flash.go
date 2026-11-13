package web

import (
	"log"
	"net/http"
)

// ── flash messages ────────────────────────────────────────────────────────────

// flash is a one-shot message shown after a redirect, so a rejected action and a
// successful one no longer produce identical screens.
type flash struct {
	Kind string // "success" | "error"
	Text string
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, text string) {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		session, _ = s.sessions.New(r, sessionName)
	}
	session.Values[sessionFlashKind] = kind
	session.Values[sessionFlashText] = text
	if err := session.Save(r, w); err != nil {
		log.Printf("flash: save: %v", err)
	}
}

func (s *Server) flashSuccess(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, "success", text)
}

func (s *Server) flashError(w http.ResponseWriter, r *http.Request, text string) {
	s.setFlash(w, r, "error", text)
}

// takeFlash reads and clears the pending message.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	session, err := s.sessions.Get(r, sessionName)
	if err != nil {
		return nil
	}
	text, _ := session.Values[sessionFlashText].(string)
	if text == "" {
		return nil
	}
	kind, _ := session.Values[sessionFlashKind].(string)

	delete(session.Values, sessionFlashText)
	delete(session.Values, sessionFlashKind)
	if err := session.Save(r, w); err != nil {
		log.Printf("flash: clear: %v", err)
	}
	return &flash{Kind: kind, Text: text}
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	session, _ := s.sessions.New(r, sessionName)
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		log.Printf("session: clear: %v", err)
	}
}

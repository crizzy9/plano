package api

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/crizzy9/plano/internal/core"
)

var friendIDPattern = regexp.MustCompile(`^[A-Z2-7]{20,128}$`)

func (s Server) registerFriendRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/friends", s.auth(s.friends))
	mux.HandleFunc("POST /v1/friends/invites", s.auth(s.createFriendInvite))
	mux.HandleFunc("POST /v1/friends/accept", s.auth(s.acceptFriendInvite))
	mux.HandleFunc("PUT /v1/friends/{id}/flights/{flightId}", s.auth(s.friendFlight))
	mux.HandleFunc("DELETE /v1/friends/{id}/flights/{flightId}", s.auth(s.friendFlight))
	mux.HandleFunc("DELETE /v1/friends/{id}", s.auth(s.removeFriend))
}

func (s Server) friends(w http.ResponseWriter, r *http.Request, owner string) error {
	view, err := s.Store.Friends(r.Context(), owner)
	if err != nil {
		return err
	}
	respond(w, http.StatusOK, view)
	return nil
}

func (s Server) createFriendInvite(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		Alias string `json:"alias"`
	}
	if err := parse(w, r, &input, 2048); err != nil {
		return err
	}
	invite, err := s.Store.CreateFriendInvite(r.Context(), owner, input.Alias, time.Now())
	if errors.Is(err, core.ErrInvalidFriend) {
		return errInvalid
	}
	if err != nil {
		return err
	}
	respond(w, http.StatusCreated, invite)
	return nil
}

func (s Server) acceptFriendInvite(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		Code  string `json:"code"`
		Alias string `json:"alias"`
	}
	if err := parse(w, r, &input, 2048); err != nil {
		return err
	}
	friend, err := s.Store.AcceptFriendInvite(r.Context(), owner, input.Code, input.Alias, time.Now())
	switch {
	case errors.Is(err, core.ErrInvalidFriend):
		return errInvalid
	case errors.Is(err, core.ErrFriendExists):
		respond(w, http.StatusConflict, map[string]string{"error": "friend relationship already exists"})
		return nil
	case err != nil:
		return err
	}
	respond(w, http.StatusCreated, struct {
		Friend core.Friend `json:"friend"`
	}{Friend: friend})
	return nil
}

func (s Server) friendFlight(w http.ResponseWriter, r *http.Request, owner string) error {
	friendID, id := r.PathValue("id"), r.PathValue("flightId")
	if !friendIDPattern.MatchString(friendID) || !flightID.MatchString(id) {
		return errInvalid
	}
	var err error
	if r.Method == http.MethodPut {
		err = s.Store.GrantFriendFlight(r.Context(), owner, friendID, id)
	} else {
		err = s.Store.RevokeFriendFlight(r.Context(), owner, friendID, id)
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s Server) removeFriend(w http.ResponseWriter, r *http.Request, owner string) error {
	id := r.PathValue("id")
	if !friendIDPattern.MatchString(id) {
		return errInvalid
	}
	if err := s.Store.RemoveFriend(r.Context(), owner, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

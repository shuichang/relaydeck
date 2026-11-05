package server

import (
	"encoding/json"
	"net/http"

	"github.com/shuichang/relaydeck/internal/paneltls"
)

func (s *Server) tlsState(w http.ResponseWriter) {
	if s.opts.TLS == nil {
		writeJSON(w, http.StatusOK, paneltls.State{Message: "当前为本地 HTTP 开发模式；HTTPS 模式支持面板证书管理。"})
		return
	}
	writeJSON(w, http.StatusOK, s.opts.TLS.State())
}

func (s *Server) putTLS(w http.ResponseWriter, r *http.Request) {
	if s.opts.TLS == nil {
		fail(w, http.StatusConflict, "当前为本地 HTTP 开发模式，未启用面板 TLS")
		return
	}
	var update paneltls.Update
	if !decode(w, r, &update) {
		return
	}
	state, err := s.opts.TLS.Apply(update)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) reloadTLS(w http.ResponseWriter, r *http.Request) {
	if s.opts.TLS == nil {
		fail(w, http.StatusConflict, "当前为本地 HTTP 开发模式，未启用面板 TLS")
		return
	}
	var body map[string]json.RawMessage
	if !decode(w, r, &body) {
		return
	}
	if body == nil || len(body) != 0 {
		fail(w, http.StatusBadRequest, "重新载入证书的请求必须是空 JSON 对象")
		return
	}
	state, err := s.opts.TLS.Reload()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

//go:build oldui

// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import (
	"context"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
	"testing"

	"github.com/bisoncraft/bosswallet/util/encode"
	"github.com/go-chi/chi/v5"
)

func TestProposalTokenCtx(t *testing.T) {
	propToken := hex.EncodeToString(encode.RandomBytes(32))
	req := (&http.Request{}).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, &chi.Context{
		URLParams: chi.RouteParams{
			Keys:   []string{"token"},
			Values: []string{propToken},
		},
	}))

	tNextHandler := &tHTTPHandler{}
	handlerFunc := proposalTokenCtx(tNextHandler)
	handlerFunc.ServeHTTP(nil, req)

	reqCtx := tNextHandler.req.Context()
	untypedToken := reqCtx.Value(ctxProposalToken)
	if untypedToken == nil {
		t.Fatalf("proposal not embedded in request context")
	}
	token, ok := untypedToken.(string)
	if !ok {
		t.Fatalf("string type assertion failed")
	}

	if token != propToken {
		t.Fatalf("wrong value embedded in request context. wanted %s, got %s", propToken, token)
	}
}

func TestGetProposalTokenCtx(t *testing.T) {
	token := encode.RandomBytes(32)
	propToken := hex.EncodeToString(token)

	r := (&http.Request{}).WithContext(context.WithValue(context.Background(), ctxProposalToken, propToken))

	extractedToken, err := getProposalTokenCtx(r)
	if err != nil {
		t.Fatalf("getProposalTokenCtx error: %v", err)
	}
	if len(extractedToken) == 0 {
		t.Fatalf("empty proposal token")
	}
	if propToken != extractedToken {
		t.Fatalf("wrong bytes. wanted %x, got %s", token, extractedToken)
	}

	// Test some negative paths
	for name, v := range map[string]any{
		"nil": nil,
		"int": 5,
	} {
		r := (&http.Request{}).WithContext(context.WithValue(context.Background(), ctxProposalToken, v))
		_, err := getProposalTokenCtx(r)
		if err == nil {
			t.Fatalf("no error for %v", name)
		}
	}
}

func TestMdToHTML(t *testing.T) {
	mdToHTML := templateFuncs["mdToHTML"].(func(string) template.HTML)

	t.Run("basic markdown", func(t *testing.T) {
		got := string(mdToHTML("# Hello\n\nWorld"))
		if !strings.Contains(got, "<h1>Hello</h1>") {
			t.Errorf("expected h1 tag, got: %s", got)
		}
		if !strings.Contains(got, "<p>World</p>") {
			t.Errorf("expected p tag, got: %s", got)
		}
	})

	t.Run("script tag is escaped (XSS prevention)", func(t *testing.T) {
		got := string(mdToHTML("<script>alert('xss')</script>"))
		if strings.Contains(got, "<script>") {
			t.Errorf("script tag should be escaped, got: %s", got)
		}
	})

	t.Run("raw HTML is escaped", func(t *testing.T) {
		got := string(mdToHTML(`<div onclick="alert('xss')">click me</div>`))
		if strings.Contains(got, "onclick") {
			t.Errorf("onclick handler should be escaped, got: %s", got)
		}
	})

	t.Run("img onerror is escaped", func(t *testing.T) {
		got := string(mdToHTML(`<img src=x onerror="alert('xss')">`))
		if strings.Contains(got, "onerror") {
			t.Errorf("onerror handler should be escaped, got: %s", got)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		got := string(mdToHTML(""))
		if strings.Contains(got, "text-danger") {
			t.Error("empty string should not produce an error")
		}
	})

	t.Run("GFM table extension works", func(t *testing.T) {
		md := "| Header |\n| --- |\n| Cell |"
		got := string(mdToHTML(md))
		if !strings.Contains(got, "<table>") {
			t.Errorf("expected table tag, got: %s", got)
		}
	})
}

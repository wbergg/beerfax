package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "report.pdf")
	pdfData := []byte("%PDF-1.4 fake content\n" + strings.Repeat("x", 200))
	if err := os.WriteFile(pdf, pdfData, 0644); err != nil {
		t.Fatal(err)
	}

	body := "3 events · stats line\n\n+----+\n| Leaderboard |\n+----+\n"
	raw, err := build(Message{
		From:        "beerfax <sender@example.com>",
		To:          []string{"a@example.com", "b@example.com"},
		Subject:     "Daily Roll 2026-07-07 — öl",
		Body:        body,
		Attachments: []string{pdf},
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	dec := new(mime.WordDecoder)
	subj, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != "Daily Roll 2026-07-07 — öl" {
		t.Fatalf("subject = %q, err=%v", subj, err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("content-type = %q, err=%v", mediaType, err)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])

	textPart, err := mr.NextPart()
	if err != nil {
		t.Fatalf("text part: %v", err)
	}
	gotBody, err := io.ReadAll(quotedprintable.NewReader(textPart))
	// Quoted-printable canonicalizes line endings to CRLF in transit.
	if err != nil || strings.ReplaceAll(string(gotBody), "\r\n", "\n") != body {
		t.Fatalf("body = %q, err=%v", gotBody, err)
	}

	attPart, err := mr.NextPart()
	if err != nil {
		t.Fatalf("attachment part: %v", err)
	}
	if ct := attPart.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/pdf") {
		t.Fatalf("attachment content-type = %q", ct)
	}
	if attPart.FileName() != "report.pdf" {
		t.Fatalf("attachment filename = %q", attPart.FileName())
	}
	gotPDF, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attPart))
	if err != nil || !bytes.Equal(gotPDF, pdfData) {
		t.Fatalf("attachment data mismatch, err=%v", err)
	}

	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("expected end of parts, got %v", err)
	}
}

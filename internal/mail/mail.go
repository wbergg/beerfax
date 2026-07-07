// Package mail sends report emails through the local msmtp binary. Messages
// are built as multipart/mixed MIME (plain-text body + attachments) and piped
// to msmtp's stdin, with recipients passed as arguments so delivery does not
// depend on header parsing.
package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Message struct {
	From        string // optional; msmtp's own config supplies the envelope sender
	To          []string
	Subject     string
	Body        string
	Attachments []string // file paths; attached as application/pdf or octet-stream
}

// Send builds the MIME message and delivers it via msmtp.
func Send(m Message) error {
	if len(m.To) == 0 {
		return fmt.Errorf("mail: no recipients")
	}
	raw, err := build(m)
	if err != nil {
		return err
	}
	args := append([]string{"--"}, m.To...)
	cmd := exec.Command("msmtp", args...)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("msmtp: %w (output: %s)", err, string(out))
	}
	return nil
}

func build(m Message) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if m.From != "" {
		fmt.Fprintf(&buf, "From: %s\r\n", m.From)
	}
	fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(m.To, ", "))
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	buf.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%q\r\n", mw.Boundary())
	buf.WriteString("\r\n")

	textHdr := textproto.MIMEHeader{}
	textHdr.Set("Content-Type", "text/plain; charset=utf-8")
	textHdr.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := mw.CreatePart(textHdr)
	if err != nil {
		return nil, fmt.Errorf("mail: text part: %w", err)
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(m.Body)); err != nil {
		return nil, fmt.Errorf("mail: text body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("mail: text body: %w", err)
	}

	for _, path := range m.Attachments {
		if err := attach(mw, path); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("mail: finalize: %w", err)
	}
	return buf.Bytes(), nil
}

func attach(mw *multipart.Writer, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mail: attachment %s: %w", path, err)
	}
	name := filepath.Base(path)
	ctype := "application/octet-stream"
	if strings.EqualFold(filepath.Ext(name), ".pdf") {
		ctype = "application/pdf"
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Type", fmt.Sprintf("%s; name=%q", ctype, name))
	hdr.Set("Content-Transfer-Encoding", "base64")
	hdr.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return fmt.Errorf("mail: attachment part: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(data)
	// RFC 2045: base64 lines must not exceed 76 characters.
	for len(enc) > 0 {
		n := min(76, len(enc))
		if _, err := part.Write([]byte(enc[:n])); err != nil {
			return fmt.Errorf("mail: attachment write: %w", err)
		}
		if _, err := part.Write([]byte("\r\n")); err != nil {
			return fmt.Errorf("mail: attachment write: %w", err)
		}
		enc = enc[n:]
	}
	return nil
}

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	r "math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	t "text/template"
	"time"
)

var unique = ""

func rand(size int) string {
	pool, output := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"), make([]rune, max(1, size))
	for index := range output {
		output[index] = pool[r.Intn(len(pool))]
	}
	return string(output)
}

func attach(path string, inline ...bool) (headers textproto.MIMEHeader, body []byte, id string, err error) {
	var content []byte

	name := ""
	if strings.HasPrefix(path, "http") {
		name = filepath.Base(strings.Split(path, "?")[0])
		client := &http.Client{Timeout: 15 * time.Second}
		if response, err := client.Get(path); err == nil {
			content, _ = io.ReadAll(response.Body)
		}
	} else {
		content, _ = os.ReadFile(path)
		name = filepath.Base(path)
	}
	if len(content) == 0 {
		return nil, nil, "", fmt.Errorf(`cannot attach "%s"`, path)
	}

	headers = textproto.MIMEHeader{}
	headers.Add("Content-Transfer-Encoding", "base64")
	headers.Add("Content-Type", http.DetectContentType(content))
	headers.Add("Content-Description", strings.TrimSuffix(name, filepath.Ext(name)))

	if len(inline) > 0 && inline[0] {
		id = rand(12) + "@" + unique
		headers.Add("Content-ID", fmt.Sprintf("<%s>", id))
		headers.Add("Content-Disposition", fmt.Sprintf(`inline; size=%d`, len(content)))
	} else {
		headers.Add("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; size=%d`, name, len(content)))
	}

	encoded, length := make([]byte, base64.StdEncoding.EncodedLen(len(content))), 76
	base64.StdEncoding.Encode(encoded, content)
	for index := 0; index < len(encoded)/length; index++ {
		body = append(body, encoded[index*length:(index+1)*length]...)
		body = append(body, byte('\n'))
	}
	if len(encoded)%length != 0 {
		body = append(body, encoded[(len(encoded)/length)*length:]...)
		body = append(body, byte('\n'))
	}
	return
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func main() {
	// load and execute template from command-line arguments
	unique = rand(12)
	if len(os.Args) < 2 {
		fatal(fmt.Errorf("usage: %s <template> [<name>=<value> ...]", filepath.Base(os.Args[0])))
	}
	buffer := &bytes.Buffer{}
	if executor, err := t.ParseFiles(os.Args[1]); err == nil {
		placeholders := map[string]any{}
		for _, argument := range os.Args[2:] {
			if parts := strings.SplitN(argument, "=", 2); len(parts) == 2 {
				parts[0], parts[1] = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
				if parts[0] != "" && parts[1] != "" {
					placeholders[parts[0]] = parts[1]
				}
			}
		}
		executor.Execute(buffer, placeholders)
	}
	template := buffer.Bytes()

	// fetch configuration from template and cleanup
	matcher, config, relay, attachments := regexp.MustCompile(`<!--\s*\[([^\]]+)\](.+?)\[/([^\]]+)\]\s*-->[\r\n]*`), map[string]string{}, "", []string{}
	if captures := matcher.FindAllSubmatch(template, -1); captures != nil {
		for _, capture := range captures {
			name := strings.TrimSpace(strings.ToLower(string(capture[1])))
			if name != "" && strings.TrimSpace(strings.ToLower(string(capture[3]))) == name {
				switch name {
				case "relay":
					relay = strings.TrimSpace(string(capture[2]))
				case "attachment":
					attachments = append(attachments, strings.TrimSpace(string(capture[2])))
				default:
					config[name] = strings.TrimSpace(string(capture[2]))
				}
			}
		}
	}
	template = matcher.ReplaceAll(template, []byte{})

	// get message configuration and enveloppe parameters
	matcher = regexp.MustCompile(`^.*?<?(\S+?@(?:[\w-]+\.)+[\w-]{2,})>?$`)
	from, mto, to := matcher.ReplaceAllString(config["from"], "${1}"), map[string]bool{}, []string{}
	for _, name := range []string{"to", "cc", "bcc"} {
		for _, value := range strings.Split(config[name], ",") {
			if value := matcher.ReplaceAllString(strings.TrimSpace(value), "${1}"); value != "" {
				mto[value] = true
			}
		}
	}
	for key := range mto {
		to = append(to, key)
	}
	if from == "" || len(to) == 0 {
		fatal(fmt.Errorf("missing mandatory configuration section"))
	}
	delete(config, "bcc")

	// build message headers
	body := &bytes.Buffer{}
	mwriter, headers := multipart.NewWriter(body), []byte{}
	for name, value := range config {
		headers = append(headers, []byte(fmt.Sprintf("%s%s: %s\r\n", strings.ToUpper(name[:1]), name[1:], value))...)
	}
	headers = append(headers, []byte("Content-Type: multipart/mixed; boundary="+mwriter.Boundary()+"\r\n")...)
	headers = append(headers, []byte("MIME-Version: 1.0\r\n")...)
	headers = append(headers, []byte("\r\n")...)

	// detect inline attachments in template
	type INLINE struct {
		headers textproto.MIMEHeader
		body    []byte
	}
	inlines := []*INLINE{}
	matcher = regexp.MustCompile(`(?: src=["']([^"']+)["']|<link .+?href=["']([^"']+)["'])`)
	if captures := matcher.FindAllSubmatchIndex(template, -1); captures != nil {
		ntemplate, start := []byte{}, 0
		for _, capture := range captures {
			for index := 2; index < len(capture); index += 2 {
				if capture[index] >= 0 && capture[index+1] >= 0 {
					pheaders, pbody, id, err := attach(string(template[capture[index]:capture[index+1]]), true)
					fatal(err)
					inlines = append(inlines, &INLINE{pheaders, pbody})
					ntemplate = append(ntemplate, template[start:capture[index]]...)
					ntemplate = append(ntemplate, []byte("cid:"+id)...)
					start = capture[index+1]
				}
			}
		}
		ntemplate = append(ntemplate, template[start:]...)
		template = ntemplate
	}

	// build first multipart/related part (with rich message + inline attachements)
	related := &bytes.Buffer{}
	rwriter := multipart.NewWriter(related)

	pheaders := textproto.MIMEHeader{}
	pheaders.Add("Content-Type", "text/html; charset=utf-8")
	part, err := rwriter.CreatePart(pheaders)
	fatal(err)
	part.Write(template)
	for _, inline := range inlines {
		part, err = rwriter.CreatePart(inline.headers)
		fatal(err)
		part.Write(inline.body)
	}
	rwriter.Close()

	// add first multipart/related part to message body
	pheaders = textproto.MIMEHeader{}
	pheaders.Add("Content-Type", "multipart/related; boundary="+rwriter.Boundary())
	part, err = mwriter.CreatePart(pheaders)
	fatal(err)
	part.Write(related.Bytes())

	// add extra attachments parts to message body
	for _, attachment := range attachments {
		pheaders, pbody, _, err := attach(attachment)
		fatal(err)
		part, err = mwriter.CreatePart(pheaders)
		fatal(err)
		part.Write(pbody)
	}
	mwriter.Close()

	// print body or send actual email
	if relay == "" {
		fmt.Printf("%s\n", append(headers, body.Bytes()...))
	} else {
		auth := []string{}
		if parts := strings.Split(relay, "|"); len(parts) >= 2 {
			if value := strings.Split(parts[0], ":"); len(value) >= 3 {
				value[0] = strings.ToLower(strings.TrimSpace(value[0]))
				if value[0] != "plain" && value[0] != "md5" {
					fatal(fmt.Errorf(`invalid smtp authentication scheme "%s"`, value[0]))
				}
				value[1] = strings.TrimSpace(value[1])
				value[2] = strings.TrimSpace(value[2])
				auth = value
			}
			relay = parts[1]
		}
		host, _, err := net.SplitHostPort(relay)
		fatal(err)
		if len(auth) >= 3 {
			if auth[0] == "plain" {
				fatal(smtp.SendMail(relay, smtp.PlainAuth("", auth[1], auth[2], host), from, to, append(headers, body.Bytes()...)))
			} else {
				fatal(smtp.SendMail(relay, smtp.CRAMMD5Auth(auth[1], auth[2]), from, to, append(headers, body.Bytes()...)))
			}
			os.Exit(0)
		}
		fatal(smtp.SendMail(relay, nil, from, to, append(headers, body.Bytes()...)))
	}
}

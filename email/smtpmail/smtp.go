package smtpmail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net"
	"strings"
	"time"

	smtpsrv "github.com/alash3al/go-smtpsrv"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/haydenwoodhead/burner.kiwi/burner"
	"github.com/haydenwoodhead/burner.kiwi/email"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

var _ burner.EmailProvider = &SMTPMail{}

type SMTPMail struct {
	srv        *smtpsrv.Server
	listenAddr string
	listener   *net.Listener
}

type handler struct {
	db            burner.Database
	isBlacklisted func(string) bool
}

func NewSMPTMailProvider(listenAddr string) *SMTPMail {
	return &SMTPMail{
		srv:        nil,
		listenAddr: listenAddr,
	}
}

func (s *SMTPMail) Start(websiteAddr string, db burner.Database, r *mux.Router, isBlacklisted func(string) bool) error {
	h := &handler{
		db:            db,
		isBlacklisted: isBlacklisted,
	}

	s.srv = &smtpsrv.Server{
		Name:        websiteAddr,
		Addr:        s.listenAddr,
		Handler:     h.handler,
		Addressable: h.addressable,
		MaxBodySize: 5 * (1024 * 1024),
	}

	go func() {
		if s.listener != nil {
			err := s.srv.Serve(*s.listener)
			if err != nil {
				log.WithError(err).Fatal("smtpmail: failed to start server")
			}
		} else {
			err := s.srv.ListenAndServe()
			if err != nil {
				log.WithError(err).Fatal("smtpmail: failed to start server")
			}
		}
	}()

	return nil
}

func (h *handler) handler(req *smtpsrv.Request) error {
	subject, err := decodeWord(req.Message.Header.Get("Subject"))
	if err != nil {
		log.WithError(err).WithField("subject", req.Message.Header.Get("Subject")).Error("smtpmail.handler: failed to decode subject")
		return err
	}

	from, err := decodeWord(req.Message.Header.Get("From"))
	if err != nil {
		log.WithError(err).WithField("from", req.Message.Header.Get("From")).Error("smtpmail.handler: failed to decode from")
		return err
	}

	partialMsg := burner.Message{
		ReceivedAt:      time.Now().Unix(),
		EmailProviderID: "smtp", // TODO: maybe a better id here? For logging purposes?
		Sender:          req.From,
		From:            from,
		Subject:         subject,
	}

	cTypeHeader := req.Message.Header.Get("Content-Type")
	if cTypeHeader == "" {
		cTypeHeader = "text/plain"
	}

	cType, params, err := mime.ParseMediaType(cTypeHeader)
	if err != nil {
		log.WithError(err).WithField("content-type-header", cTypeHeader).Error("smtpmail.handler: failed to parse message media type")
		return fmt.Errorf("smtp.handler: failed to parse message media type: %w", err)
	}

	if strings.HasPrefix(cType, "text/plain") {
		bb, err := ioutil.ReadAll(req.Message.Body)
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to read email body")
			return fmt.Errorf("smtp.handler: failed to read text email body: %w", err)
		}

		partialMsg.BodyPlain = string(bytes.TrimSpace(bb))
	} else if strings.HasPrefix(cType, "text/html") {
		bb, err := ioutil.ReadAll(req.Message.Body)
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to read email body")
			return fmt.Errorf("smtp.handler: failed to read html email body: %w", err)
		}

		modifiedHTML, err := email.AddTargetBlank(string(bb))
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to AddTargetBlank")
			return fmt.Errorf("smtp.handler: failed to AddTargetBlank: %w", err)
		}

		partialMsg.BodyHTML = modifiedHTML
	} else if strings.HasPrefix(cType, "multipart/") {
		messageCopy, err := ioutil.ReadAll(req.Message.Body)
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to read email body for copy")
			return fmt.Errorf("smtp.handler: failed to read email body: %w", err)
		}

		copyReader := bytes.NewReader(messageCopy)

		text, html, err := extractParts(copyReader, params["boundary"])
		if err != nil {
			log.WithError(err).WithField("message", string(messageCopy)).Error("smtpmail.handler: failed to parse multipart")
			return err
		}

		partialMsg.BodyPlain = strings.TrimSpace(text)
		partialMsg.BodyHTML = strings.TrimSpace(html)
	}

	for _, rcpt := range req.To {
		inbox, err := h.db.GetInboxByAddress(rcpt)
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to retrieve inbox")
			return fmt.Errorf("smtp.handler: failed to retrieve inbox: %w", err)
		}

		mID, err := uuid.NewRandom()
		if err != nil {
			log.WithError(err).Printf("smtpmail.handler: failed to generate uuid for inbox")
			return fmt.Errorf("smtp.handler: failed to generate uuid for inbox: %w", err)
		}

		msg := partialMsg
		msg.ID = mID.String()
		msg.InboxID = inbox.ID
		msg.TTL = inbox.TTL

		err = h.db.SaveNewMessage(msg)
		if err != nil {
			log.WithError(err).Error("smtpmail.handler: failed to save message to db")
			return fmt.Errorf("smtp.handler: failed to save message to db: %w", err)
		}
	}

	return nil
}

func extractParts(r io.Reader, boundary string) (string, string, error) {
	var text, html string
	mr := multipart.NewReader(r, boundary)

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return text, html, nil
		} else if err != nil {
			return "", "", fmt.Errorf("smtp.extractParts: failed to failed to get next part: %w", err)
		}

		cType := p.Header.Get("Content-Type")

		bb, err := ioutil.ReadAll(p)

		if strings.HasPrefix(cType, "text/plain") {
			text = string(bb)
		} else if strings.HasPrefix(cType, "text/html") {
			trimmed := bytes.TrimSpace(bb)
			modifiedHTML, err := email.AddTargetBlank(string(trimmed))
			if err != nil {
				return "", "", fmt.Errorf("smtp.extractParts: failed to AddTargetBlank: %w", err)
			}

			html = modifiedHTML
		} else {
			continue
		}

		if err != nil {
			if err == io.ErrUnexpectedEOF {
				return text, html, nil
			}
			return "", "", fmt.Errorf("smtp.extractParts: failed to read email body: %w", err)
		}
	}
}

var wordDecoder = new(mime.WordDecoder)

func decodeWord(word string) (string, error) {
	if strings.HasPrefix(word, "=?") {
		dec, err := wordDecoder.DecodeHeader(word)
		if err != nil {
			return "", errors.Wrap(err, "smtp.decodeWord: failed to decode")
		}
		return dec, nil
	}
	return word, nil
}

func (h *handler) addressable(user, address string) bool {
	if h.isBlacklisted(address) {
		return false
	}

	exists, err := h.db.EmailAddressExists(address)
	if err != nil {
		log.WithError(err).Printf("smtp.addressable: failed to query if email exists")
		return false
	}

	return exists
}

func (s *SMTPMail) Stop() error {
	return s.srv.Shutdown(context.Background())
}

// RegisterRoute is redundant in this instance as we're not calling to an external service to register a callback
// instead we will receive all email and then be asking our db directly if we should accept this email or not.
func (s *SMTPMail) RegisterRoute(i burner.Inbox) (string, error) {
	return "smtp", nil
}

package payload

import (
	"github.com/boeq/mail-to-webhook/internal/mailparse"
)

// Summary is what the history shows of a message without building its payload.
type Summary struct {
	FromEmail   string
	FromName    string
	To          string
	Subject     string
	MessageID   string
	Attachments []AttachmentSummary
}

type AttachmentSummary struct {
	Name        string
	ContentType string
	Size        int
}

// Summarise reads the display fields of a parsed message.
func Summarise(m *mailparse.Message) Summary {
	s := Summary{
		To:        clean(mailparse.DecodeWords(m.Get("To"))),
		Subject:   clean(m.GetDecoded("Subject")),
		MessageID: clean(trimAngles(m.Get("Message-ID"))),
	}
	if from := parseList(m.Get("From")); len(from) > 0 {
		s.FromEmail = clean(from[0].Email)
		s.FromName = clean(from[0].Name)
	}
	for _, a := range m.Attachments {
		if len(a.Data) == 0 {
			continue
		}
		s.Attachments = append(s.Attachments, AttachmentSummary{
			Name:        attachmentName(a),
			ContentType: clean(a.ContentType),
			Size:        len(a.Data),
		})
	}
	return s
}

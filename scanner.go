package testresults

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Case struct {
	ClassName string  `json:"class_name"`
	File      string  `json:"file"`
	Name      string  `json:"name"`
	Result    string  `json:"result"`
	Duration  float64 `json:"duration"`
	Message   string  `json:"message"`
}

type Scanner struct {
	done chan error
	t    *tokenizer

	caseReady chan *caseWithErr

	// some cases to swap around to avoid allocations
	cur  *caseWithErr
	next *caseWithErr
	swp  *caseWithErr
}

// Scan returns the next case or nil if an error
// Any previous value of a returned case is invalid after a subsequent call to Scan
// and if it is needed the caller must copy it.
// To capture all errors keep scanning until case and error are nil. Doing that will
// also guarantee no goroutines have been leaked
func (s *Scanner) Scan() (*Case, error) {
	select {
	case err := <-s.done:
		close(s.caseReady)
		return nil, err
	case test := <-s.caseReady:
		return &test.Case, test.err
	}
}

type caseWithErr struct {
	Case
	err error
}

func (s *Scanner) Start(ctx context.Context, file io.Reader) {
	s.caseReady = make(chan *caseWithErr)

	s.done = make(chan error)

	// assign the case to build
	s.cur = new(caseWithErr)
	s.next = new(caseWithErr)

	ctx, cancel := context.WithCancel(ctx)

	s.t = newTokenizer(ctx, file)

	go func() {
		if err := s.process(); err != nil {
			cancel() // tels the tokenizer to stop
			s.done <- err
		}
		close(s.done)
	}()

}

type tokenType int

const (
	start tokenType = iota
	end
	char
)

type token struct {
	which tokenType

	elem string

	attr map[string]string
	data []byte
}

func (s *Scanner) process() error {
	for {
		tok, err := s.t.next()
		if err != nil || tok == nil {
			return err
		}
		switch tok.which {
		case start:
			switch tok.elem {
			case "testsuites":
				if err := s.testSuites(); err != nil {
					return err
				}
			case "testsuite":
				if err := s.testSuite(tok.attr); err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid top level element: %s", tok.elem)
			}
		case end:
			return fmt.Errorf("invalid top level end element: %s", tok.elem)
		}
	}
}

func (s *Scanner) testSuites() error {
	for {
		tok, err := s.t.next()
		switch {
		case err != nil:
			return err
		case tok == nil:
			return errors.New("end of token stream without closing testsuites element")
		case tok.which == start:
			switch tok.elem {
			case "testsuite":
				if err := s.testSuite(tok.attr); err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid testsuites element: %s", tok.elem)
			}
		case tok.which == end:
			return nil
		}
	}
}

func (s *Scanner) testSuite(attr map[string]string) error {
	for {
		tok, err := s.t.next()
		switch {
		case err != nil:
			return err
		case tok == nil:
			return errors.New("end of token stream without closing testsuite element")
		case tok.which == start:
			switch tok.elem {
			case "testcase": // minOccurs="0" maxOccurs="unbounded"/>
				if err := s.testcase(attr["file"], attr["name"], tok.attr); err != nil {
					return err
				}
			case "properties", "system-out", "system-err": // minOccurs="0" maxOccurs="1"/>
				if err := discard(s.t); err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid testsuite element: %s", tok.elem)
			}
		case tok.which == end:
			return nil
		}
	}
}

// nolint:gocyclo
// it has necessary complexity
func (s *Scanner) testcase(file, suiteName string, attr map[string]string) (err error) {
	// default to the file from the suite
	// but if a file is specified on the test case use that
	if attr["file"] != "" {
		file = attr["file"]
	}

	// default to the suite name for the class
	// but if a class is specified on the test case use that
	classname := suiteName
	if attr["classname"] != "" {
		classname = attr["classname"]
	}

	duration, _ := strconv.ParseFloat(attr["time"], 64)

	s.cur.Name = attr["name"]
	s.cur.Result = "success"
	s.cur.ClassName = classname
	s.cur.File = file
	s.cur.Duration = duration
	s.cur.Message = ""

	failed := false
	skipped := false

	// prioritise failure over skipped and both of those over system-*
	defer func() {
		switch {
		case err != nil:
			return
		case failed:
			s.cur.Result = "failed"
		case skipped:
			s.cur.Result = "skipped"
		}

		// flip cur to be the old complete
		s.swp = s.next
		s.next = s.cur
		s.cur = s.swp
		s.caseReady <- s.next
	}()

	for {
		tok, err := s.t.next()
		if err != nil {
			return err
		}
		if tok == nil {
			return errors.New("end of token stream without closing testcase element")
		}
		switch tok.which {
		case start:
			switch tok.elem {
			case "skipped": // minOccurs="0" maxOccurs="1"/>
				// (no need to assert on one skipped element)
				skipped = true
			case "error": // minOccurs="0" maxOccurs="unbounded"/>
				failed = true
			case "failure": // minOccurs="0" maxOccurs="unbounded"/>
				failed = true
			case "system-out": // minOccurs="0" maxOccurs="unbounded"/>
				s.cur.Result = tok.elem
			case "system-err": // minOccurs="0" maxOccurs="unbounded"/>
				s.cur.Result = tok.elem
			default:
				return fmt.Errorf("%s, invalid testcase element: %s", s.cur.Name, tok.elem)
			}
			message, err := parseMessage(s.t, tok.attr["message"])
			if err != nil {
				return fmt.Errorf("%s, %w", s.cur.Name, err)
			}
			s.cur.Message += message
		case end:
			return nil
		}
	}
}

// nolint:gocyclo
// it has necessary complexity
func parseMessage(t *tokenizer, message string) (result string, err error) {
	content := ""

	defer func() {
		switch {
		case err != nil:
			result = ""
		case message == "" && content == "":
			result = ""
		case message != "" && content == "":
			result = message
		case message == "" && content != "":
			result = content
		case strings.Contains(strings.Replace(content, " ", "", -1), strings.Replace(message, " ", "", -1)):
			result = content
		default:
			result = message + "\n" + content
		}
	}()

	for {
		tok, err := t.next()
		if err != nil {
			return content, err
		}
		if tok == nil {
			return "", errors.New("end of token stream without closing element")
		}
		switch tok.which {
		case start:
			return "", errors.New("unexpected element")
		case end:
			return "", nil
		case char:
			if tok.data != nil {
				content += strings.TrimSpace(string(tok.data))
			}
		}
	}
}

// discard recursively ignores all elements
func discard(t *tokenizer) error {
	for {
		tok, err := t.next()
		switch {
		case err != nil:
			return err
		case tok == nil:
			return errors.New("end of token stream without closing element")
		case tok.which == start:
			if err := discard(t); err != nil {
				return err
			}
		case tok.which == end:
			return nil
		}
	}
}

type tokenizer struct {
	ctx context.Context
	d   *xml.Decoder
	tok token
}

func newTokenizer(ctx context.Context, reader io.Reader) *tokenizer {
	return &tokenizer{
		ctx: ctx,
		d:   xml.NewDecoder(reader),
		tok: token{ // reuse the same token to avoid allocations
			attr: make(map[string]string),
			data: make([]byte, 65536), // preallocate a pretty big buffer
		},
	}
}

func (t tokenizer) next() (*token, error) {
	for {
		if t.ctx.Err() != nil {
			return nil, t.ctx.Err()
		}
		// Read tokens from the XML document in a stream.
		tok, err := t.d.RawToken()
		if tok == nil || err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// Inspect the type of the token just read.
		switch el := tok.(type) {
		case xml.StartElement:
			t.tok.elem = strings.ToLower(el.Name.Local)
			t.tok.which = start
			for _, a := range el.Attr {
				t.tok.attr[strings.ToLower(a.Name.Local)] = a.Value
			}
			return &t.tok, nil
		case xml.CharData:
			if len(el) > 0 {
				t.tok.which = char
				t.tok.data = t.tok.data[:0]
				t.tok.data = append(t.tok.data, el...)
				return &t.tok, nil
			}
		case xml.EndElement:
			t.tok.elem = strings.ToLower(el.Name.Local)
			t.tok.which = end
			return &t.tok, nil
		}
	}
}

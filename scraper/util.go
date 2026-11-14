package scraper

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

var templateRe = regexp.MustCompile(`\{\{\s*(\w+)\s*(:(\w+))?\s*\}\}`)

// missingParamValue marks a template variable that is known to be
// absent. It lets chain Param mode distinguish "upstream had no such
// field" from "upstream supplied an empty string".
type missingParamValue struct{}

var missingParamSentinel any = missingParamValue{}

func template(isurl bool, str string, vars map[string]any) (string, error) {
	var err error
	out := templateRe.ReplaceAllStringFunc(str, func(key string) string {
		if err != nil {
			return ""
		}
		m := templateRe.FindStringSubmatch(key)
		k := m[1]
		value, ok := vars[k]
		if ok {
			if _, missing := value.(missingParamValue); missing {
				ok = false
			}
		}
		if !ok {
			if m[3] != "" {
				value = m[3]
			} else {
				err = errors.New("missing param: " + k)
				return ""
			}
		}
		s, ok := value.(string)
		if !ok {
			s = fmt.Sprint(value)
		}
		if isurl {
			queryi := strings.Index(str, "?")
			keyi := strings.Index(str, key)
			if queryi != -1 && keyi > queryi {
				s = url.QueryEscape(s)
			}
		}
		return s
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

func checkSelector(s string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	doc, _ := goquery.NewDocumentFromReader(bytes.NewBufferString(`<html>
		<body>
			<h3>foo bar</h3>
		</body>
	</html>`))
	doc.Find(s)
	return
}

func jsonerr(err error) []byte {
	return []byte(`{"error":"` + err.Error() + `"}`)
}

func logf(format string, args ...interface{}) {
	log.Printf("[scraper] "+format, args...)
}

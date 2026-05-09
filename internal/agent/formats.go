package agent

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"time"
)

func validateFormat(format, value string) error {
	switch format {
	case "":
		return nil
	case "email":
		_, err := mail.ParseAddress(value)
		return err
	case "uuid":
		if regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`).MatchString(value) {
			return nil
		}
		return fmt.Errorf("must be uuid")
	case "date":
		_, err := time.Parse("2006-01-02", value)
		return err
	case "date-time":
		_, err := time.Parse(time.RFC3339, value)
		return err
	case "uri":
		u, err := url.Parse(value)
		if err != nil {
			return err
		}
		if u.Scheme == "" {
			return fmt.Errorf("must include URI scheme")
		}
		return nil
	default:
		return fmt.Errorf("unsupported format")
	}
}

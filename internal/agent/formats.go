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
		if _, err := mail.ParseAddress(value); err != nil {
			return fmt.Errorf("must be email")
		}
		return nil
	case "uuid":
		if regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`).MatchString(value) {
			return nil
		}
		return fmt.Errorf("must be uuid")
	case "date":
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return fmt.Errorf("must be date")
		}
		return nil
	case "date-time":
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("must be date-time")
		}
		return nil
	case "uri":
		u, err := url.Parse(value)
		if err != nil {
			return fmt.Errorf("must be uri")
		}
		if u.Scheme == "" {
			return fmt.Errorf("must include URI scheme")
		}
		return nil
	default:
		return fmt.Errorf("unsupported format")
	}
}

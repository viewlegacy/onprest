package main

import (
	"bytes"
	"context"
	"encoding/csv"

	onprest "github.com/viewlegacy/onprest/sdk/go"
)

type Input struct {
	Customer string `json:"customer"`
}

func main() {
	onprest.Run(func(ctx context.Context, in Input) (onprest.Result, error) {
		var buf bytes.Buffer
		writer := csv.NewWriter(&buf)
		if err := writer.Write([]string{"customer"}); err != nil {
			return onprest.Result{}, err
		}
		if err := writer.Write([]string{in.Customer}); err != nil {
			return onprest.Result{}, err
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			return onprest.Result{}, err
		}
		onprest.Log("info", "generated customer.csv")
		return onprest.Result{
			Data: map[string]any{"customer": in.Customer},
			Files: []onprest.File{
				{Name: "customer.csv", MIMEType: "text/csv", Reader: &buf},
			},
		}, nil
	})
}

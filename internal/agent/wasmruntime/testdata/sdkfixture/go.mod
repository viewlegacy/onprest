module github.com/viewlegacy/onprest/sdkfixture

go 1.26.9

require (
	github.com/viewlegacy/onprest/sdk/go v1.0.0
	golang.org/x/oauth2 v0.35.0
)

replace github.com/viewlegacy/onprest/sdk/go => ../../../../../sdk/go

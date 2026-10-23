// Test-only fake backend for deploy/canary/test.sh. A separate module so it
// stays out of the service's ./... build, vet and test runs.
module github.com/go-tangra/go-tangra-sms-gw/v4/deploy/canary/fakebackend

go 1.26.3

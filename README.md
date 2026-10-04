# Tangra SMS Gateway V4

Implementation is in progress from `specs/001-sms-gateway-v4/tasks.md`. The source gateway at `../go-tangra-sms-gw` is read-only. See the feature specification and `docs/compatibility.md` for preserved public behavior and intentional security changes.

Go module uses the local Tangra V4 baseline; UI uses the V4 kit and federation. Configuration, runtime services, database migration, public routes and management pages will be delivered in task order. The empty UI remote currently provides no management functions.

```sh
go mod download
make test
make build
cd ui
npm install
npm run build
```

UI package installation needs `NODE_AUTH_TOKEN` with read access to @go-tangra/ui. No token, key, certificate or legacy data belongs in source control. `make generate`, `make compose-up` and integration targets require later implementation artifacts; they are not acceptance evidence yet.

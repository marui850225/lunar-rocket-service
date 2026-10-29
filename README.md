# Rockets Service

Consumes rocket messages on `POST /messages` and serves rocket state over REST.

Work in progress. Run, test and design sections will be filled in as the implementation lands.

## Requirements

- Go 1.27+

## Commands

```bash
make run    # start on :8088
make test   # unit tests with the race detector
make e2e    # real rockets binary against the service
```

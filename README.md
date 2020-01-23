# GO-LIBP2P-INTROSPECTION

![CircleCI](https://circleci.com/gh/nearform/go-libp2p-introspection.svg?style=svg&circle-token=8dd2dc607fcfd1b496346ed8af22ca3ad74a358c)

A go module allowing users to extract, stream and save data from their p2p peer 

## Getting Started

TODO

### Prerequisites

To use this module you must have Golang installed. Since this uses modules you will need a Go version > 1.11. This module specifically targets 1.13.

```
brew install golang
```

You must install the required dependencies for this module using the following:

```
make deps
```

### Running


```
go run ./cmd/server/*.go
```

An example UI lives at https://nearform.github.io/introspection-ui

## Running the tests

Tests are expected to be run with `gotestsum`, a Go test runner written by CircleCI that outputs useful artifacts for our CI system. To run these tests, use the following:

```
make test
```

Tests can also be run with the following without any dependency:

```
go test ./...
```

## Compile the introspection.proto

```
protoc --go_out=. introspection.proto
```

### Break down into end to end tests

Explain what these tests test and why

```
Give an example
```

### Code styling and Linting

Go comes with a built in code formatter. Code should adhere to this standard. Run this with the following:

```
go fmt ./...
```

## Deployment

Add additional notes about how to deploy this on a live system


## Contributing

Please read [CONTRIBUTING.md] for details on our code of conduct, and the process for submitting pull requests to us.

## Versioning

We use [SemVer](http://semver.org/) for versioning. For the versions available, see the [tags on this repository](https://github.com/your/project/tags). 

## Authors

* **Ron Litzenberger** - *Initial work* - 

See also the list of [contributors](https://github.com/your/project/contributors) who participated in this project.

## License

This project is licensed under the MIT License - see the [LICENSE.md](LICENSE.md) file for details

## Acknowledgments


package port

import "errors"

// errEmptySymbol is returned for an empty symbol by any resolver in this package. Defined once so
// callers can errors.Is it regardless of which resolver produced it.
var errEmptySymbol = errors.New("empty symbol: no instrument id can be resolved")

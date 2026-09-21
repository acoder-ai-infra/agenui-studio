// Package examples exposes the small, runnable assets used by local
// initialization. Production code reads these exact sources, so the examples
// cannot drift from the system operator versions published at startup.
package examples

import _ "embed"

//go:embed operators/format_money.ts
var FormatMoneyOperator string

//go:embed operators/format_distance.ts
var FormatDistanceOperator string

//go:embed operators/list_join.ts
var ListJoinOperator string

//go:embed operators/list_sum.ts
var ListSumOperator string

//go:embed operators/list_max.ts
var ListMaxOperator string

//go:embed operators/list_pick.ts
var ListPickOperator string

//go:embed operators/list_filter.ts
var ListFilterOperator string

//go:embed operators/list_map.ts
var ListMapOperator string

//go:embed products.json
var ProductsJSON []byte

//go:embed product-api.json
var ProductAPIJSON []byte

//go:embed offers.json
var OffersJSON []byte

//go:embed offer-api.json
var OfferAPIJSON []byte

//go:embed travel-status.json
var TravelStatusJSON []byte

//go:embed travel-status-api.json
var TravelStatusAPIJSON []byte

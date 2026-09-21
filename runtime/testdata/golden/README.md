# Runtime Golden Packages

These ten positive Card Execution Package `2.0` files are designed for manual
review with the real Runtime CLI.

Start the deterministic fixture server in terminal 1:

```bash
cd /path/to/agenui-studio
go run ./runtime/cmd/agenui-runtime-fixture-server
```

Run the packages in terminal 2:

```bash
cd /path/to/agenui-studio

go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/01-fixed-map-key.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/02-fixed-array-index.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/03-wildcard-coordinate-map.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/04-list-join.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/05-list-sum.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/06-list-max.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/07-pick-format-money-chain.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/08-filter-map-join-chain.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/09-nested-wildcards.json
go run ./runtime/cmd/agenui-runtime/main.go ./runtime/testdata/golden/10-empty-list.json
```

Expected DataModels:

| Case | Expected DataModel |
| --- | --- |
| 01 | `{"product_price":"¥68.00"}` |
| 02 | `{"product_price":"¥68.00"}` |
| 03 | Two mapped items with `¥68.00` and `¥128.00` |
| 04 | `{"tag_summary":"亲子 · 室内 · 免费"}` |
| 05 | `{"total":6}` |
| 06 | `{"maximum":9}` |
| 07 | `{"product_price":"¥68.00"}` |
| 08 | `{"open_names":"A,C"}` |
| 09 | Two-level coordinates with `¥68.00`, `¥128.00`, and `¥99.00` |
| 10 | `{"items":[]}` with no operator invocation and no panic |

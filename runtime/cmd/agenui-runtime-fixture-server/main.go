package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	responses := map[string]string{
		"/case01": `{"price_map":{"vip-member":6800}}`,
		"/case02": `{"items":[{"price_cents":6800},{"price_cents":12800}]}`,
		"/case03": `{"items":[{"title":"Museum","price_cents":6800},{"title":"Lake","price_cents":12800}]}`,
		"/case04": `{"tags":["亲子","室内","免费"]}`,
		"/case05": `{"values":[1,2,3]}`,
		"/case06": `{"values":[1,9,3]}`,
		"/case07": `{"items":[{"id":"p1","price_cents":6800},{"id":"p2","price_cents":12800}]}`,
		"/case08": `{"items":[{"status":"open","name":"A"},{"status":"closed","name":"B"},{"status":"open","name":"C"}]}`,
		"/case09": `{"groups":[{"items":[{"price_cents":6800},{"price_cents":12800}]},{"items":[{"price_cents":9900}]},{"items":[]}]}`,
		"/case10": `{"items":[]}`,
	}
	mux := http.NewServeMux()
	for path, body := range responses {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintln(w, body)
		})
	}
	log.Printf("Runtime fixture server listening on http://127.0.0.1:18094")
	log.Fatal(http.ListenAndServe("127.0.0.1:18094", mux))
}

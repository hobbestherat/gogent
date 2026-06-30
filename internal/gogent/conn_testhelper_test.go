package gogent

import (
	"gogent/internal/config"
	"gogent/internal/model"
)

// testConn builds a routable, no-auth model connection pointed at the local
// placeholder (api_type "openai" + DefaultModelURL). Tests then repoint it with
// SetURL(serverURL). It replaces the old `model.NewModelConnection(nil, nil)`
// idiom: a nil ProviderConnection is now (correctly) unroutable in production — a
// model referencing a missing connection must fail clearly rather than silently
// dial localhost — so tests build an explicit local connection instead.
func testConn() *model.ModelConnection {
	return model.NewModelConnection(
		&config.ProviderConnection{APIType: "openai", Endpoint: model.DefaultModelURL},
		nil,
	)
}

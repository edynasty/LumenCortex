package agent

import (
	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

const testRef = config.ModelRef("test/model")

// testModels is the one-model catalog most loop tests run on: testRef, tool calling, the default limits.
func testModels(p protocol.Provider) map[config.ModelRef]Model {
	return map[config.ModelRef]Model{testRef: testModel(testRef, p)}
}

func testModel(ref config.ModelRef, p protocol.Provider) Model {
	return Model{
		Ref: ref, ProviderID: "test", ModelID: p.Model(),
		Context: config.DefaultContext, Output: config.DefaultOutput, ToolCalling: true, Client: p,
	}
}

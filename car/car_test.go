package car

import (
	"testing"

	"github.com/fission-codes/go-car-mirror/dag"
)

func TestCreateCar(t *testing.T) {
	cidStrings := []string{"bafybeigs3wowz6pug7ckfgtwrsrltjjx5disx5pztnucgt4ygryv5w6qy4"}
	cids, _ := dag.ParseCids(cidStrings)
	CreateCar(cids)
}
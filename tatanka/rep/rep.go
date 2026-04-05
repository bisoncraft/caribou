// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rep

import (
	"fmt"
	"math"
)

type interactionClass uint16

const (
	iClassMatchRequest interactionClass = iota
	iClassTrade
)

var interactionClasses = map[interactionClass]*struct {
	name  string
	slots uint8
}{
	iClassMatchRequest: {
		name:  "MatchRequest",
		slots: 18,
	},
	iClassTrade: {
		name:  "Trade",
		slots: 64,
	},
}

func (ia interactionClass) String() string {
	if i, found := interactionClasses[ia]; found {
		return i.name
	}
	return "UNDEFINED_INTERACTION_CLASS"
}

type Experience uint32

const (
	ExperienceMatchRequestIgnored Experience = iota
)

type experienceDefinition struct {
	name   string
	class  interactionClass
	impact int8
	weight uint8
}

var culture = map[Experience]*experienceDefinition{
	ExperienceMatchRequestIgnored: {
		name:   "MatchRequestIgnored",
		class:  iClassMatchRequest,
		impact: -2,
		weight: 2,
	},
}

func (oID Experience) String() string {
	if o, found := culture[oID]; found {
		return o.name
	}
	return "UNKNOWN_OUTCOME"
}

type memory struct {
	exp    Experience
	expDef *experienceDefinition
}

type Memories map[interactionClass][]*memory

func (mems Memories) Remember(exp Experience) error {
	expDef, found := culture[exp]
	if !found {
		return fmt.Errorf("unknown outcome %d", exp)
	}
	i, found := interactionClasses[expDef.class]
	if !found {
		return fmt.Errorf("unknown interaction class %d for outcome %s", expDef.class, exp)
	}
	outcomeLog := mems[expDef.class]
	if outcomeLog == nil {
		outcomeLog = make([]*memory, 0, i.slots)
		mems[expDef.class] = outcomeLog
	}
	outcomeLog = append(outcomeLog, &memory{
		exp:    exp,
		expDef: expDef,
	})
	if n := len(outcomeLog); n > int(i.slots) {
		outcomeLog = outcomeLog[n-int(i.slots):]
		mems[expDef.class] = outcomeLog
	}
	return nil
}

func (mems Memories) ReputationScore() int8 {
	var weightSum, totalImpact int64
	for _, ms := range mems {
		for _, mem := range ms {
			weightSum += int64(mem.expDef.weight)
			totalImpact += int64(mem.expDef.weight) * int64(mem.expDef.impact)
		}
	}
	ratio := float64(totalImpact) / float64(weightSum)
	return int8(math.Round(ratio * 127))
}

type Mems struct {
	ShortTerm []int
	LongTerm  []int
	Fleeting  []int
	Traumatic []int
}

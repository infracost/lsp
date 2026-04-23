package scanner

import (
	"testing"

	goprotoevent "github.com/infracost/go-proto/pkg/event"
	"github.com/infracost/go-proto/pkg/rat"
	"github.com/infracost/proto/gen/go/infracost/parser/event"
	"github.com/infracost/proto/gen/go/infracost/rational"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ratProto(s string) *rational.Rat {
	r, err := rat.NewFromString(s)
	if err != nil {
		panic(s)
	}
	return r.Proto()
}

func TestEvaluateGuardrails_NoGuardrails(t *testing.T) {
	s := &Scanner{}
	result := s.EvaluateGuardrails([]goprotoevent.ProjectCostInfo{
		{ProjectName: "prod", TotalMonthlyCost: rat.New(500)},
	})
	assert.Nil(t, result)
}

func TestEvaluateGuardrails_TotalThresholdTriggered(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:             "g1",
				Name:           "Monthly spend limit",
				Scope:          event.Guardrail_REPO,
				TotalThreshold: ratProto("1000"),
				BlockPr:        true,
				Message:        "Cost exceeds $1000/month",
			},
		},
	}

	// Past cost was below threshold, head cost is above — should trigger.
	projects := []goprotoevent.ProjectCostInfo{
		{
			ProjectName:          "prod",
			TotalMonthlyCost:     rat.New(1200),
			PastTotalMonthlyCost: rat.New(800),
		},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 1)
	assert.True(t, results[0].Triggered)
	assert.True(t, results[0].BlockPR)
	assert.Equal(t, "Monthly spend limit", results[0].GuardrailName)
	assert.Equal(t, "Cost exceeds $1000/month", results[0].Message)
}

func TestEvaluateGuardrails_TotalThresholdNotCrossed(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:             "g1",
				Name:           "Monthly spend limit",
				Scope:          event.Guardrail_REPO,
				TotalThreshold: ratProto("1000"),
			},
		},
	}

	// Both past and head are above threshold — no crossing, should not trigger.
	projects := []goprotoevent.ProjectCostInfo{
		{
			ProjectName:          "prod",
			TotalMonthlyCost:     rat.New(1200),
			PastTotalMonthlyCost: rat.New(1100),
		},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 1)
	assert.False(t, results[0].Triggered)
}

func TestEvaluateGuardrails_IncreaseThresholdTriggered(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:                "g2",
				Name:              "Large increase",
				Scope:             event.Guardrail_REPO,
				IncreaseThreshold: ratProto("200"),
			},
		},
	}

	projects := []goprotoevent.ProjectCostInfo{
		{
			ProjectName:          "prod",
			TotalMonthlyCost:     rat.New(700),
			PastTotalMonthlyCost: rat.New(400),
		},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 1)
	assert.True(t, results[0].Triggered)
}

func TestEvaluateGuardrails_MultipleProjects(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:             "g1",
				Name:           "Repo total",
				Scope:          event.Guardrail_REPO,
				TotalThreshold: ratProto("1000"),
			},
		},
	}

	// Two projects: combined total crosses threshold.
	projects := []goprotoevent.ProjectCostInfo{
		{ProjectName: "prod", TotalMonthlyCost: rat.New(700), PastTotalMonthlyCost: rat.New(400)},
		{ProjectName: "staging", TotalMonthlyCost: rat.New(400), PastTotalMonthlyCost: rat.New(200)},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 1)
	assert.True(t, results[0].Triggered)
}

func TestEvaluateGuardrails_MultipleGuardrails(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:             "g1",
				Name:           "Total limit",
				Scope:          event.Guardrail_REPO,
				TotalThreshold: ratProto("1000"),
			},
			{
				Id:                "g2",
				Name:              "Increase limit",
				Scope:             event.Guardrail_REPO,
				IncreaseThreshold: ratProto("50"),
			},
		},
	}

	projects := []goprotoevent.ProjectCostInfo{
		{ProjectName: "prod", TotalMonthlyCost: rat.New(1200), PastTotalMonthlyCost: rat.New(800)},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 2)

	triggered := make(map[string]bool)
	for _, r := range results {
		triggered[r.GuardrailName] = r.Triggered
	}
	assert.True(t, triggered["Total limit"], "total limit should trigger")
	assert.True(t, triggered["Increase limit"], "increase limit should trigger")
}

func TestEvaluateGuardrails_NilResourceCosts(t *testing.T) {
	s := &Scanner{
		guardrails: []*event.Guardrail{
			{
				Id:             "g1",
				Name:           "Total limit",
				Scope:          event.Guardrail_REPO,
				TotalThreshold: ratProto("100"),
			},
		},
	}

	// nil TotalMonthlyCost should be treated as zero without panicking.
	projects := []goprotoevent.ProjectCostInfo{
		{ProjectName: "prod", TotalMonthlyCost: nil, PastTotalMonthlyCost: rat.Zero},
	}

	results := s.EvaluateGuardrails(projects)
	require.Len(t, results, 1)
	assert.False(t, results[0].Triggered)
}

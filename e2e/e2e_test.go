package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/brianvoe/gofakeit/v6"
	cloudevents "github.com/cloudevents/sdk-go/v2/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/openmeterio/openmeter/api/client/go"
	credit_model "github.com/openmeterio/openmeter/internal/credit"
	"github.com/openmeterio/openmeter/pkg/models"
)

func initClient(t *testing.T) *api.ClientWithResponses {
	t.Helper()

	address := os.Getenv("OPENMETER_ADDRESS")
	if address == "" {
		t.Skip("OPENMETER_ADDRESS not set")
	}

	client, err := api.NewClientWithResponses(address)
	require.NoError(t, err)

	return client
}

func TestMain(m *testing.M) {
	// Make sure OpenMeter is ready
	// TODO: replace this with some sort of health check
	time.Sleep(15 * time.Second)

	os.Exit(m.Run())
}

func TestIngest(t *testing.T) {
	client := initClient(t)

	// Make clickhouse's job easier by sending events within a fix time range
	now := time.Now()

	var sum int

	for i := 0; i < 1000; i++ {
		// Make clickhouse's job easier by sending events within a fix time range
		timestamp := gofakeit.DateRange(now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
		duration := gofakeit.Number(1, 100)

		sum += duration

		ev := cloudevents.New()
		ev.SetID(gofakeit.UUID())
		ev.SetSource("my-app")
		ev.SetType("ingest")
		ev.SetSubject("customer-1")
		ev.SetTime(timestamp)
		_ = ev.SetData("application/json", map[string]string{
			"duration_ms": fmt.Sprintf("%d", duration),
		})

		resp, err := client.IngestEventWithResponse(context.Background(), ev)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode())
	}

	// Wait for events to be processed
	assert.EventuallyWithT(t, func(t *assert.CollectT) {
		resp, err := client.QueryMeterWithResponse(context.Background(), "ingest", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())

		require.Len(t, resp.JSON200.Data, 1)
		assert.Equal(t, float64(sum), resp.JSON200.Data[0].Value)
	}, 30*time.Second, time.Second)
}

func TestBatchIngest(t *testing.T) {
	client := initClient(t)

	// Make clickhouse's job easier by sending events within a fix time range
	now := time.Now()

	var sum int

	var events []cloudevents.Event

	for i := 0; i < 1000; i++ {
		// Make clickhouse's job easier by sending events within a fix time range
		timestamp := gofakeit.DateRange(now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))
		duration := gofakeit.Number(1, 1000)

		sum += duration

		ev := cloudevents.New()
		ev.SetID(gofakeit.UUID())
		ev.SetSource("my-app")
		ev.SetType("batchingest")
		ev.SetSubject("customer-1")
		ev.SetTime(timestamp)
		_ = ev.SetData("application/json", map[string]string{
			"duration_ms": fmt.Sprintf("%d", duration),
		})

		events = append(events, ev)
	}

	resp, err := client.IngestEventBatchWithResponse(context.Background(), events)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode())

	// Wait for events to be processed
	assert.EventuallyWithT(t, func(t *assert.CollectT) {
		resp, err := client.QueryMeterWithResponse(context.Background(), "batchingest", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())

		require.Len(t, resp.JSON200.Data, 1)
		assert.Equal(t, float64(sum), resp.JSON200.Data[0].Value)
	}, 30*time.Second, time.Second)
}

func TestDedupe(t *testing.T) {
	client := initClient(t)

	// Make clickhouse's job easier by sending events within a fix time range
	now := time.Now()

	for i := 0; i < 1000; i++ {
		// Make clickhouse's job easier by sending events within a fix time range
		timestamp := gofakeit.DateRange(now.Add(-30*24*time.Hour), now.Add(30*24*time.Hour))

		ev := cloudevents.New()
		ev.SetID("52f44f66-020f-4fa9-a733-102a8ef6f515")
		ev.SetSource("my-app")
		ev.SetType("dedupe")
		ev.SetSubject("customer-1")
		ev.SetTime(timestamp)
		_ = ev.SetData("application/json", map[string]string{})

		resp, err := client.IngestEventWithResponse(context.Background(), ev)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode())
	}

	// Wait for events to be processed
	assert.EventuallyWithT(t, func(t *assert.CollectT) {
		resp, err := client.QueryMeterWithResponse(context.Background(), "dedupe", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())

		require.Len(t, resp.JSON200.Data, 1)
		assert.Equal(t, 1.0, resp.JSON200.Data[0].Value)
	}, 30*time.Second, time.Second)
}

func TestQuery(t *testing.T) {
	client := initClient(t)

	// Reproducible random data
	const customerCount = 5
	paths := []string{"/", "/about", "/users", "/contact"}
	faker := gofakeit.New(8675309)
	timestamp := faker.DateRange(time.Date(2023, time.May, 6, 0, 0, 0, 0, time.UTC), faker.FutureDate().UTC()).UTC().Truncate(time.Second)

	t.Run("Total", func(t *testing.T) {
		var events []cloudevents.Event

		newEvents := func(fn func(ev *cloudevents.Event)) []cloudevents.Event {
			var events []cloudevents.Event

			for i := 0; i < customerCount; i++ {
				ev := cloudevents.New()

				ev.SetID(faker.UUID())
				ev.SetSource("my-app")
				ev.SetType("query")
				ev.SetSubject(fmt.Sprintf("customer-%d", i+1))

				fn(&ev)

				events = append(events, ev)
			}

			return events
		}

		newTimedEvents := func(timestamp time.Time) []cloudevents.Event {
			method := faker.HTTPMethod()
			path := paths[faker.Number(0, len(paths)-1)]

			return newEvents(func(ev *cloudevents.Event) {
				ev.SetTime(timestamp)
				_ = ev.SetData("application/json", map[string]string{
					"duration_ms": "100",
					"method":      method,
					"path":        path,
				})
			})
		}

		// First event
		{
			events = append(events, newTimedEvents(timestamp)...)
		}

		// Plus one minute
		{
			events = append(events, newTimedEvents(timestamp.Add(time.Minute))...)
		}

		// Plus one hour
		{
			events = append(events, newTimedEvents(timestamp.Add(time.Hour))...)
		}

		// Plus one day
		{
			events = append(events, newTimedEvents(timestamp.Add(24*time.Hour))...)
		}

		{
			resp, err := client.IngestEventBatchWithResponse(context.Background(), events)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, resp.StatusCode())
		}

		// Wait for events to be processed
		assert.EventuallyWithT(t, func(t *assert.CollectT) {
			resp, err := client.QueryMeterWithResponse(context.Background(), "query", nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode())

			expected := &api.MeterQueryResult{
				Data: []models.MeterQueryRow{
					{
						Value:       customerCount * 4 * 100,
						WindowStart: timestamp.Truncate(time.Minute),
						WindowEnd:   timestamp.Add(24 * time.Hour).Truncate(time.Minute).Add(time.Minute),
						GroupBy:     map[string]*string{},
					},
				},
			}

			assert.Equal(t, expected, resp.JSON200)
		}, 30*time.Second, time.Second)
	})

	t.Run("WindowSize", func(t *testing.T) {
		t.Parallel()

		t.Run("Minute", func(t *testing.T) {
			t.Parallel()

			windowSize := models.WindowSizeMinute

			// Wait for events to be processed
			assert.EventuallyWithT(t, func(t *assert.CollectT) {
				resp, err := client.QueryMeterWithResponse(context.Background(), "query", &api.QueryMeterParams{
					WindowSize: &windowSize,
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode())

				expected := &api.MeterQueryResult{
					Data: []models.MeterQueryRow{
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Truncate(time.Minute),
							WindowEnd:   timestamp.Truncate(time.Minute).Add(time.Minute),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(time.Minute).Truncate(time.Minute),
							WindowEnd:   timestamp.Add(time.Minute).Truncate(time.Minute).Add(time.Minute),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(time.Hour).Truncate(time.Minute),
							WindowEnd:   timestamp.Add(time.Hour).Truncate(time.Minute).Add(time.Minute),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(24 * time.Hour).Truncate(time.Minute),
							WindowEnd:   timestamp.Add(24 * time.Hour).Truncate(time.Minute).Add(time.Minute),
							GroupBy:     map[string]*string{},
						},
					},
					WindowSize: &windowSize,
				}

				assert.Equal(t, expected, resp.JSON200)
			}, 30*time.Second, time.Second)
		})

		t.Run("Hour", func(t *testing.T) {
			t.Parallel()

			windowSize := models.WindowSizeHour

			// Wait for events to be processed
			assert.EventuallyWithT(t, func(t *assert.CollectT) {
				resp, err := client.QueryMeterWithResponse(context.Background(), "query", &api.QueryMeterParams{
					WindowSize: &windowSize,
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode())

				expected := &api.MeterQueryResult{
					Data: []models.MeterQueryRow{
						{
							Value:       customerCount * 2 * 100,
							WindowStart: timestamp.Truncate(time.Hour),
							WindowEnd:   timestamp.Truncate(time.Hour).Add(time.Hour),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(time.Hour).Truncate(time.Hour),
							WindowEnd:   timestamp.Add(time.Hour).Truncate(time.Hour).Add(time.Hour),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(24 * time.Hour).Truncate(time.Hour),
							WindowEnd:   timestamp.Add(24 * time.Hour).Truncate(time.Hour).Add(time.Hour),
							GroupBy:     map[string]*string{},
						},
					},
					WindowSize: &windowSize,
				}

				assert.Equal(t, expected, resp.JSON200)
			}, 30*time.Second, time.Second)
		})

		t.Run("Day", func(t *testing.T) {
			t.Parallel()

			windowSize := models.WindowSizeDay

			// Wait for events to be processed
			assert.EventuallyWithT(t, func(t *assert.CollectT) {
				resp, err := client.QueryMeterWithResponse(context.Background(), "query", &api.QueryMeterParams{
					WindowSize: &windowSize,
				})
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode())

				expected := &api.MeterQueryResult{
					Data: []models.MeterQueryRow{
						{
							Value:       customerCount * 3 * 100,
							WindowStart: timestamp.Truncate(24 * time.Hour),
							WindowEnd:   timestamp.Truncate(24 * time.Hour).Add(24 * time.Hour),
							GroupBy:     map[string]*string{},
						},
						{
							Value:       customerCount * 100,
							WindowStart: timestamp.Add(24 * time.Hour).Truncate(24 * time.Hour),
							WindowEnd:   timestamp.Add(24 * time.Hour).Truncate(24 * time.Hour).Add(24 * time.Hour),
							GroupBy:     map[string]*string{},
						},
					},
					WindowSize: &windowSize,
				}

				assert.Equal(t, expected, resp.JSON200)
			}, 30*time.Second, time.Second)
		})
	})

	t.Run("Subject", func(t *testing.T) {
		t.Parallel()

		// TODO: randomize?
		// TODO: make sure we have enough subject
		subject := []string{"customer-1", "customer-2"}

		// Wait for events to be processed
		assert.EventuallyWithT(t, func(t *assert.CollectT) {
			resp, err := client.QueryMeterWithResponse(context.Background(), "query", &api.QueryMeterParams{
				Subject: &subject,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode())

			expected := &api.MeterQueryResult{
				Data: []models.MeterQueryRow{
					{
						Value:       4 * 100,
						WindowStart: timestamp.Truncate(time.Minute),
						WindowEnd:   timestamp.Truncate(time.Minute).Add(24*time.Hour + time.Minute),
						Subject:     &subject[1],
						GroupBy:     map[string]*string{},
					},
					{
						Value:       4 * 100,
						WindowStart: timestamp.Truncate(time.Minute),
						WindowEnd:   timestamp.Truncate(time.Minute).Add(24*time.Hour + time.Minute),
						Subject:     &subject[0],
						GroupBy:     map[string]*string{},
					},
				},
			}

			assert.Equal(t, expected, resp.JSON200)
		}, 30*time.Second, time.Second)
	})

	// TODO: improve group by tests by adding more than one parameter
	//
	// Note: this test breaks if any of the randomization parameters are changed
	// TODO: Fix query ordering first
	// t.Run("GroupBy", func(t *testing.T) {
	// 	t.Parallel()
	//
	// 	resp, err := client.QueryMeterWithResponse(context.Background(), "query", &api.QueryMeterParams{
	// 		GroupBy: &[]string{"method"},
	// 	})
	// 	require.NoError(t, err)
	// 	require.Equal(t, http.StatusOK, resp.StatusCode())
	//
	// 	expected := &api.MeterQueryResult{
	// 		Data: []models.MeterQueryRow{
	// 			{
	// 				Value:       4 * 100,
	// 				WindowStart: timestamp.Truncate(time.Minute),
	// 				WindowEnd:   timestamp.Truncate(time.Minute).Add(24*time.Hour + time.Minute),
	// 				GroupBy:     map[string]*string{},
	// 			},
	// 			{
	// 				Value:       4 * 100,
	// 				WindowStart: timestamp.Truncate(time.Minute),
	// 				WindowEnd:   timestamp.Truncate(time.Minute).Add(24*time.Hour + time.Minute),
	// 				GroupBy:     map[string]*string{},
	// 			},
	// 		},
	// 	}
	//
	// 	assert.Equal(t, expected, resp.JSON200)
	// })

	// TODO: add tests for group by and subject
}

func TestCredit(t *testing.T) {
	client := initClient(t)
	subject := "customer-1"
	meterSlug := "credit_test_meter"

	t.Run("Ingest usage", func(t *testing.T) {
		// Reproducible random data
		faker := gofakeit.New(8675309)
		var events []cloudevents.Event

		newEvent := func(timestamp string, model string) cloudevents.Event {
			ts, err := time.Parse(time.RFC3339, timestamp)
			if err != nil {
				t.Fatal(err)
			}

			ev := cloudevents.New()
			ev.SetID(faker.UUID())
			ev.SetSource("credit-test")
			ev.SetType("credit_event")
			ev.SetTime(ts)
			ev.SetSubject(subject)
			_ = ev.SetData("application/json", map[string]string{
				"model": model,
			})

			return ev
		}

		// First event
		{
			events = append(events, newEvent("2024-01-01T00:01:00Z", "gpt-4"))
		}

		// Irrelevant event (does not affect credit because of model mismatch)
		{
			events = append(events, newEvent("2024-01-01T00:01:00Z", "gpt-3"))
		}

		// Ingore events
		{
			resp, err := client.IngestEventBatchWithResponse(context.Background(), events)
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, resp.StatusCode())
		}

		// Wait for events to be processed
		assert.EventuallyWithT(t, func(t *assert.CollectT) {
			resp, err := client.QueryMeterWithResponse(context.Background(), meterSlug, &api.QueryMeterParams{
				Subject: &[]string{subject},
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode())

			// As we invested two events with a count meter
			assert.Equal(t, 2.0, resp.JSON200.Data[0].Value)
		}, 30*time.Second, time.Second)
	})

	t.Run("Create Feature", func(t *testing.T) {
		resp, err := client.CreateFeatureWithResponse(context.Background(), api.Feature{
			Name:      "Credit Test Feature",
			MeterSlug: "credit_test_meter",
			MeterGroupByFilters: &map[string]string{
				"model": "gpt-4",
			},
		})

		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode(), "Invalid status code [response_body=%s]", resp.Body)

		featureId := resp.JSON201.ID
		archived := false
		expected := &api.Feature{
			ID:        featureId,
			Name:      "Credit Test Feature",
			MeterSlug: "credit_test_meter",
			MeterGroupByFilters: &map[string]string{
				"model": "gpt-4",
			},
			Archived: &archived,
		}

		assert.Equal(t, expected, resp.JSON201)
	})

	t.Run("Create Grant", func(t *testing.T) {
		effectiveAt, _ := time.Parse(time.RFC3339, "2024-01-01T00:01:00Z")

		// Get feature
		featureListResp, err := client.ListFeaturesWithResponse(context.Background())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, featureListResp.StatusCode())
		require.NotNil(t, featureListResp.JSON200)
		require.Len(t, *featureListResp.JSON200, 1)
		features := *featureListResp.JSON200
		featureId := features[0].ID

		// Create grant
		resp, err := client.CreateCreditGrantWithResponse(context.Background(), subject, api.CreateCreditGrantRequest{
			Type:        credit_model.GrantTypeUsage,
			FeatureID:   featureId,
			Amount:      100,
			Priority:    1,
			EffectiveAt: effectiveAt,
			Rollover: &api.CreditGrantRollover{
				Type: credit_model.GrantRolloverTypeRemainingAmount,
			},
			Expiration: api.CreditExpirationPeriod{
				Duration: "DAY",
				Count:    1,
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode(), "Invalid status code [response_body=%s]", resp.Body)

		expected := &api.CreditGrantResponse{
			ID:          resp.JSON201.ID,
			Subject:     subject,
			Type:        credit_model.GrantTypeUsage,
			FeatureID:   featureId,
			Amount:      100,
			Priority:    1,
			EffectiveAt: effectiveAt,
			Rollover: &api.CreditGrantRollover{
				Type: credit_model.GrantRolloverTypeRemainingAmount,
			},
			Expiration: api.CreditExpirationPeriod{
				Duration: "DAY",
				Count:    1,
			},
		}

		assert.Equal(t, expected, resp.JSON201)
	})

	t.Run("Balance", func(t *testing.T) {
		// Get grants
		grantListResp, err := client.ListCreditGrantsWithResponse(context.Background(), &api.ListCreditGrantsParams{})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, grantListResp.StatusCode())
		require.NotNil(t, grantListResp.JSON200)
		require.Len(t, *grantListResp.JSON200, 1)
		grants := *grantListResp.JSON200
		grant := grants[0]

		// Get feature
		featureListResp, err := client.ListFeaturesWithResponse(context.Background())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, featureListResp.StatusCode())
		require.NotNil(t, featureListResp.JSON200)
		require.Len(t, *featureListResp.JSON200, 1)
		features := *featureListResp.JSON200
		feature := features[0]

		resp, err := client.GetCreditBalanceWithResponse(context.Background(), subject, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())

		expected := &api.CreditBalance{
			Subject: subject,
			FeatureBalances: []credit_model.FeatureBalance{
				{
					Feature: feature,
					Balance: 99,
				},
			},
			GrantBalances: []credit_model.GrantBalance{
				{
					Grant:   grant,
					Balance: 99,
				},
			},
		}

		assert.Equal(t, expected, resp.JSON200)
	})

	t.Run("Reset", func(t *testing.T) {
		effectiveAt, _ := time.ParseInLocation(time.RFC3339, "2024-01-01T00:02:00Z", time.UTC)

		// Get grants
		parentGrantListResp, err := client.ListCreditGrantsWithResponse(context.Background(), &api.ListCreditGrantsParams{})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, parentGrantListResp.StatusCode())
		require.NotNil(t, parentGrantListResp.JSON200)
		require.Len(t, *parentGrantListResp.JSON200, 1)
		parentGrants := *parentGrantListResp.JSON200
		parentGrant := parentGrants[0]

		// Get feature
		featureListResp, err := client.ListFeaturesWithResponse(context.Background())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, featureListResp.StatusCode())
		require.NotNil(t, featureListResp.JSON200)
		require.Len(t, *featureListResp.JSON200, 1)
		features := *featureListResp.JSON200
		featureId := features[0].ID

		// Reset credit
		resetResp, err := client.ResetCreditWithResponse(context.Background(), subject, api.CreditReset{
			EffectiveAt: effectiveAt,
		})

		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resetResp.StatusCode())

		reset := resetResp.JSON201
		expectedReset := &api.CreditReset{
			ID:          reset.ID,
			Subject:     subject,
			EffectiveAt: effectiveAt,
		}
		assert.Equal(t, expectedReset, resetResp.JSON201)

		// List grants
		resp, err := client.ListCreditGrantsWithResponse(context.Background(), &api.ListCreditGrantsParams{
			Subject: &[]string{subject},
		})

		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())

		grants := *resp.JSON200
		expected := &[]api.CreditGrantResponse{
			{
				ID:        grants[0].ID,
				ParentID:  parentGrant.ID,
				Subject:   subject,
				Type:      credit_model.GrantTypeUsage,
				FeatureID: featureId,
				Amount:    99,
				Priority:  1,
				// TODO: this should be equal to `effectiveAt` but we run into timezone issues
				EffectiveAt: grants[0].EffectiveAt,
				Rollover: &api.CreditGrantRollover{
					Type: credit_model.GrantRolloverTypeRemainingAmount,
				},
				Expiration: api.CreditExpirationPeriod{
					Duration: "DAY",
					Count:    1,
				},
			},
		}

		assert.Equal(t, expected, resp.JSON200)
	})
}

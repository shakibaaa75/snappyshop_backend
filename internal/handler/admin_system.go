package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"shopbd/backend/internal/httpx"
	"shopbd/backend/internal/metrics"
)

// SystemHandler serves the admin "System health" report:
//
//	GET /api/v1/admin/system
//
// It combines server (process + machine), database and traffic numbers.
type SystemHandler struct {
	DB      *mongo.Database
	Metrics *metrics.Collector
	Log     *slog.Logger
}

const systemReportTimeout = 8 * time.Second

// Report — GET /api/v1/admin/system
func (h *SystemHandler) Report(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), systemReportTimeout)
	defer cancel()

	rt := metrics.ReadRuntime(h.Metrics.Started())
	traffic := h.Metrics.Snapshot()
	db := h.database(ctx)

	status, issues := assessHealth(rt, traffic, db)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"health":      map[string]any{"status": status, "issues": issues},
		"server":      rt,
		"traffic":     traffic,
		"database":    db,
	})
}

// assessHealth turns the numbers into one status (ok | warning | critical)
// and a list of plain-language problems.
func assessHealth(rt metrics.Runtime, t metrics.Snapshot, db map[string]any) (string, []string) {
	status := "ok"
	issues := []string{}
	raise := func(level, msg string) {
		if level == "critical" || status == "ok" {
			status = level
		}
		issues = append(issues, msg)
	}

	if ok, _ := db["ok"].(bool); !ok {
		raise("critical", "Database is not reachable")
	} else if ms, _ := db["pingMs"].(float64); ms > 300 {
		raise("warning", fmt.Sprintf("Database is slow to respond (%.0f ms)", ms))
	}
	if t.Last5Minutes >= 20 && t.Last5ErrorRate > 5 {
		raise("warning", fmt.Sprintf("%.1f%% of requests failed with a server error in the last 5 minutes", t.Last5ErrorRate))
	}
	if t.P95Ms >= 1000 && t.TotalRequests >= 50 {
		raise("warning", fmt.Sprintf("Slow responses: 95%% of requests finish within %.0f ms", t.P95Ms))
	}
	if rt.Host != nil {
		if rt.Host.MemUsedPct > 90 {
			raise("warning", fmt.Sprintf("Server memory is %.0f%% used", rt.Host.MemUsedPct))
		}
		if rt.CPUs > 0 && rt.Host.Load5 > float64(rt.CPUs)*1.5 {
			raise("warning", fmt.Sprintf("Server load is high (%.2f on %d CPUs)", rt.Host.Load5, rt.CPUs))
		}
	}
	if rt.Goroutines > 10000 {
		raise("warning", fmt.Sprintf("Unusually many background tasks running (%d goroutines)", rt.Goroutines))
	}
	return status, issues
}

// database gathers MongoDB health. Every part is optional: managed or
// restricted MongoDB users may not be allowed to run serverStatus/dbStats,
// in which case that section is simply left out.
func (h *SystemHandler) database(ctx context.Context) map[string]any {
	res := map[string]any{"name": h.DB.Name()}

	start := time.Now()
	if err := h.DB.Client().Ping(ctx, readpref.Primary()); err != nil {
		if h.Log != nil {
			h.Log.Error("system report: database ping failed", "error", err.Error())
		}
		res["ok"] = false
		return res
	}
	res["ok"] = true
	res["pingMs"] = float64(time.Since(start).Microseconds()) / 1000

	var ss bson.M
	if err := h.DB.RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&ss); err == nil {
		res["serverStatus"] = true
		if v, ok := ss["version"].(string); ok {
			res["version"] = v
		}
		res["uptimeSeconds"] = bsonNum(ss["uptime"])
		res["connections"] = map[string]any{
			"current":      bsonNum(bsonPath(ss, "connections", "current")),
			"available":    bsonNum(bsonPath(ss, "connections", "available")),
			"totalCreated": bsonNum(bsonPath(ss, "connections", "totalCreated")),
		}
		res["opcounters"] = map[string]any{
			"insert":  bsonNum(bsonPath(ss, "opcounters", "insert")),
			"query":   bsonNum(bsonPath(ss, "opcounters", "query")),
			"update":  bsonNum(bsonPath(ss, "opcounters", "update")),
			"delete":  bsonNum(bsonPath(ss, "opcounters", "delete")),
			"getmore": bsonNum(bsonPath(ss, "opcounters", "getmore")),
			"command": bsonNum(bsonPath(ss, "opcounters", "command")),
		}
		res["memory"] = map[string]any{
			"residentMb": bsonNum(bsonPath(ss, "mem", "resident")),
			"virtualMb":  bsonNum(bsonPath(ss, "mem", "virtual")),
		}
		res["network"] = map[string]any{
			"bytesIn":     bsonNum(bsonPath(ss, "network", "bytesIn")),
			"bytesOut":    bsonNum(bsonPath(ss, "network", "bytesOut")),
			"numRequests": bsonNum(bsonPath(ss, "network", "numRequests")),
		}
	} else {
		res["serverStatus"] = false
	}

	var ds bson.M
	if err := h.DB.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&ds); err == nil {
		res["stats"] = map[string]any{
			"collections":   bsonNum(ds["collections"]),
			"objects":       bsonNum(ds["objects"]),
			"indexes":       bsonNum(ds["indexes"]),
			"dataSizeBytes": bsonNum(ds["dataSize"]),
			"storageBytes":  bsonNum(ds["storageSize"]),
			"indexBytes":    bsonNum(ds["indexSize"]),
		}
	}

	if names, err := h.DB.ListCollectionNames(ctx, bson.D{}); err == nil {
		type row struct {
			Name  string `json:"name"`
			Count int64  `json:"documents"`
		}
		rows := make([]row, 0, len(names))
		for _, n := range names {
			c, err := h.DB.Collection(n).EstimatedDocumentCount(ctx)
			if err != nil {
				continue
			}
			rows = append(rows, row{Name: n, Count: c})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
		res["collections"] = rows
	}
	return res
}

// bsonPath walks nested documents (bson.M, bson.D or map[string]any).
func bsonPath(v any, keys ...string) any {
	for _, k := range keys {
		switch m := v.(type) {
		case bson.M:
			v = m[k]
		case map[string]any:
			v = m[k]
		case bson.D:
			var found any
			for _, e := range m {
				if e.Key == k {
					found = e.Value
					break
				}
			}
			v = found
		default:
			return nil
		}
	}
	return v
}

// bsonNum converts any BSON number to float64 (0 when it is not a number).
func bsonNum(v any) float64 {
	switch n := v.(type) {
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

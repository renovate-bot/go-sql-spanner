// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spannerdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	pb "cloud.google.com/go/spanner/testdata/protos"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	"github.com/googleapis/go-sql-spanner/testutil"
	lru "github.com/hashicorp/golang-lru/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	gstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestPingContext(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("unexpected error for ping: %v", err)
	}
}

func TestPingContext_Fails(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	s := gstatus.Newf(codes.PermissionDenied, "Permission denied for database")
	_ = server.TestSpanner.PutStatementResult("SELECT 1", &testutil.StatementResult{Err: s.Err()})
	if g, w := db.PingContext(context.Background()), driver.ErrBadConn; g != w {
		t.Fatalf("ping error mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestStatementCacheSize(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnectionWithParams(t, "StatementCacheSize=2")
	defer teardown()

	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for connection: %v", err)
	}
	var cache *lru.Cache[string, *statementsCacheEntry]
	if err := c.Raw(func(driverConn any) error {
		sc, ok := driverConn.(*conn)
		if !ok {
			return fmt.Errorf("driverConn is not a Spanner conn")
		}
		cache = sc.parser.statementsCache
		return nil
	}); err != nil {
		t.Fatalf("unexpected error for raw: %v", err)
	}

	// The cache should initially be empty.
	if g, w := cache.Len(), 0; g != w {
		t.Fatalf("cache size mismatch\n Got: %v\nWant: %v", g, w)
	}

	for n := 0; n < 3; n++ {
		rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Executing the same query multiple times should add the statement once to the cache.
	if g, w := cache.Len(), 1; g != w {
		t.Fatalf("cache size mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Executing another statement should add yet another statement to the cache.
	if _, err := db.ExecContext(context.Background(), testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	if g, w := cache.Len(), 2; g != w {
		t.Fatalf("cache size mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Executing yet another statement should evict the oldest result from the cache and add this.
	query := "insert into test (id) values (1)"
	_ = server.TestSpanner.PutStatementResult(query, &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	if _, err := db.ExecContext(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	// The cache size should still be 2.
	if g, w := cache.Len(), 2; g != w {
		t.Fatalf("cache size mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestDisableStatementCache(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnectionWithParams(t, "DisableStatementCache=true")
	defer teardown()

	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("unexpected error for connection: %v", err)
	}
	var cache *lru.Cache[string, *statementsCacheEntry]
	if err := c.Raw(func(driverConn any) error {
		sc, ok := driverConn.(*conn)
		if !ok {
			return fmt.Errorf("driverConn is not a Spanner conn")
		}
		cache = sc.parser.statementsCache
		return nil
	}); err != nil {
		t.Fatalf("unexpected error for raw: %v", err)
	}

	// There should be no cache.
	if cache != nil {
		t.Fatalf("statement cache should be disabled")
	}

	// Executing queries and other statements should work without a cache.
	for n := 0; n < 3; n++ {
		rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
}

func TestSimpleQuery(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for want := int64(1); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"FOO"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse() == nil {
		t.Fatalf("missing single use selector for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse().GetReadOnly() == nil {
		t.Fatalf("missing read-only option for ExecuteSqlRequest")
	}
	if !req.Transaction.GetSingleUse().GetReadOnly().GetStrong() {
		t.Fatalf("missing strong timestampbound for ExecuteSqlRequest")
	}
}

func TestDirectExecuteQuery(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	// This does not use DirectExecuteQuery. The query is only sent to Spanner when
	// rows.Next is called.
	rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	// There should be no request on the server.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 0; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if !rows.Next() {
		t.Fatal("no rows")
	}
	// The request should now be present on the server.
	requests = drainRequestsFromServer(server.TestSpanner)
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	_ = rows.Close()

	// Now repeat the same with the DirectExecuteQuery option.
	rows, err = db.QueryContext(context.Background(), testutil.SelectFooFromBar, ExecOptions{DirectExecuteQuery: true})
	if err != nil {
		t.Fatal(err)
	}
	// The request should be present on the server.
	requests = drainRequestsFromServer(server.TestSpanner)
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	// Verify that we can get the row that we selected.
	if !rows.Next() {
		t.Fatal("no rows")
	}
	_ = rows.Close()
}

func TestConcurrentScanAndClose(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()
	rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}

	// Only fetch the first row of the query to make sure that the rows are not auto-closed
	// when the end of the stream is reached.
	rows.Next()
	var got int64
	err = rows.Scan(&got)
	if err != nil {
		t.Fatal(err)
	}

	// Close both the database and the rows (connection) in parallel.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = db.Close()
	}()
	go func() {
		defer wg.Done()
		_ = rows.Close()
	}()
	wg.Wait()
}

func TestSingleQueryWithTimestampBound(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SET READ_ONLY_STALENESS = 'MAX_STALENESS 10s'"); err != nil {
		t.Fatalf("Set read-only staleness: %v", err)
	}
	rows, err := conn.QueryContext(context.Background(), testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}

	for rows.Next() {
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	_ = rows.Close()
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse() == nil {
		t.Fatalf("missing single use selector for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse().GetReadOnly() == nil {
		t.Fatalf("missing read-only option for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse().GetReadOnly().GetMaxStaleness() == nil {
		t.Fatalf("missing max_staleness timestampbound for ExecuteSqlRequest")
	}

	// Close the connection and execute a new query. This should use a strong read.
	_ = conn.Close()
	rows, err = db.QueryContext(context.Background(), testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}

	for rows.Next() {
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	_ = rows.Close()
	requests = drainRequestsFromServer(server.TestSpanner)
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req = sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse() == nil {
		t.Fatalf("missing single use selector for ExecuteSqlRequest")
	}
	if req.Transaction.GetSingleUse().GetReadOnly() == nil {
		t.Fatalf("missing read-only option for ExecuteSqlRequest")
	}
	if !req.Transaction.GetSingleUse().GetReadOnly().GetStrong() {
		t.Fatalf("missing strong timestampbound for ExecuteSqlRequest")
	}
}

func TestSimpleReadOnlyTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for want := int64(1); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"FOO"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	err = tx.Commit()
	if err != nil {
		t.Fatal(err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	// Read-only transactions are not really committed on Cloud Spanner, so
	// there should be no commit request on the server.
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 0; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	beginReadOnlyRequests := filterBeginReadOnlyRequests(requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{})))
	if g, w := len(beginReadOnlyRequests), 0; g != w {
		t.Fatalf("begin requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestReadOnlyTransactionWithStaleness(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SET READ_ONLY_STALENESS = 'EXACT_STALENESS 10s'"); err != nil {
		t.Fatalf("Set read-only staleness: %v", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}

	for rows.Next() {
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	_ = rows.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	beginReadOnlyRequests := filterBeginReadOnlyRequests(requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{})))
	if g, w := len(beginReadOnlyRequests), 0; g != w {
		t.Fatalf("begin requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	executeRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(executeRequests), 1; g != w {
		t.Fatalf("execute requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	executeReq := executeRequests[0].(*sppb.ExecuteSqlRequest)
	if executeReq.GetTransaction() == nil || executeReq.GetTransaction().GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	if executeReq.GetTransaction().GetBegin().GetReadOnly().GetExactStaleness() == nil {
		t.Fatalf("missing exact_staleness option on BeginTransaction option")
	}
}

func TestReadOnlyTransactionWithOptions(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	// Set max open connections to 1 to force a failure if there is a connection leak.
	db.SetMaxOpenConns(1)

	tx, err := BeginReadOnlyTransaction(ctx, db, ReadOnlyTransactionOptions{
		TimestampBound:         spanner.ExactStaleness(10 * time.Second),
		BeginTransactionOption: spanner.InlinedBeginTransaction,
	})
	if err != nil {
		t.Fatal(err)
	}
	useTx := func(tx *sql.Tx) {
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			t.Fatal(err)
		}

		for rows.Next() {
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		_ = rows.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	useTx(tx)

	requests := drainRequestsFromServer(server.TestSpanner)
	beginReadOnlyRequests := filterBeginReadOnlyRequests(requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{})))
	if g, w := len(beginReadOnlyRequests), 0; g != w {
		t.Fatalf("begin requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	executeRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(executeRequests), 1; g != w {
		t.Fatalf("execute requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	executeReq := executeRequests[0].(*sppb.ExecuteSqlRequest)
	if executeReq.GetTransaction() == nil || executeReq.GetTransaction().GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	if executeReq.GetTransaction().GetBegin().GetReadOnly().GetExactStaleness() == nil {
		t.Fatalf("missing exact_staleness option on BeginTransaction option")
	}

	// Verify that the staleness option is not 'sticky' on the database.
	// Running a second transaction also verifies that there is no connection
	// leak, as the database has max one connection in the pool.
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	useTx(tx)

	requests = drainRequestsFromServer(server.TestSpanner)
	beginReadOnlyRequests = filterBeginReadOnlyRequests(requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{})))
	if g, w := len(beginReadOnlyRequests), 0; g != w {
		t.Fatalf("begin requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	executeRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(executeRequests), 1; g != w {
		t.Fatalf("execute requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	executeReq = executeRequests[0].(*sppb.ExecuteSqlRequest)
	if executeReq.GetTransaction() == nil || executeReq.GetTransaction().GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	if executeReq.GetTransaction().GetBegin().GetReadOnly().GetExactStaleness() != nil {
		t.Fatalf("got unexpected exact_staleness option on BeginTransaction request")
	}
}

func TestSimpleReadWriteTransaction(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(conn)
	if _, err := conn.ExecContext(ctx, "set max_commit_delay='10ms'"); err != nil {
		t.Fatal(err)
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for want := int64(1); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"FOO"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	err = tx.Commit()
	if err != nil {
		t.Fatal(err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	beginRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{}))
	if g, w := len(beginRequests), 0; g != w {
		t.Fatalf("begin requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	if req.LastStatement {
		t.Fatalf("last statement set for ExecuteSqlRequest")
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitReq := commitRequests[0].(*sppb.CommitRequest)
	if g, w := commitReq.MaxCommitDelay.Nanos, int32(time.Millisecond*10); g != w {
		t.Fatalf("max_commit_delay mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestPreparedQuery(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	_ = server.TestSpanner.PutStatementResult(
		"SELECT * FROM Test WHERE Id=@id",
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateSelect1ResultSet(),
		},
	)

	stmt, err := db.Prepare("SELECT * FROM Test WHERE Id=@id")
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(stmt)
	rows, err := stmt.Query(1)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for want := int64(1); rows.Next(); want++ {
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.ParamTypes), 1; g != w {
		t.Fatalf("param types length mismatch\nGot: %v\nWant: %v", g, w)
	}
	if pt, ok := req.ParamTypes["id"]; ok {
		if g, w := pt.Code, sppb.TypeCode_INT64; g != w {
			t.Fatalf("param type mismatch\nGot: %v\nWant: %v", g, w)
		}
	} else {
		t.Fatalf("no param type found for @id")
	}
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params length mismatch\nGot: %v\nWant: %v", g, w)
	}
	if val, ok := req.Params.Fields["id"]; ok {
		if g, w := val.GetStringValue(), "1"; g != w {
			t.Fatalf("param value mismatch\nGot: %v\nWant: %v", g, w)
		}
	} else {
		t.Fatalf("no value found for param @id")
	}
}

func TestQueryWithAllTypes(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	query := `SELECT *
             FROM Test
             WHERE ColBool=@bool 
             AND   ColString=@string
             AND   ColBytes=@bytes
             AND   ColInt=@int64
             AND   ColFloat32=@float32
             AND   ColFloat64=@float64
             AND   ColNumeric=@numeric
             AND   ColDate=@date
             AND   ColTimestamp=@timestamp
             AND   ColJson=@json
             AND   ColUuid=@uuid
             AND   ColBoolArray=@boolArray
             AND   ColStringArray=@stringArray
             AND   ColBytesArray=@bytesArray
             AND   ColIntArray=@int64Array
             AND   ColFloat32Array=@float32Array
             AND   ColFloat64Array=@float64Array
             AND   ColNumericArray=@numericArray
             AND   ColDateArray=@dateArray
             AND   ColTimestampArray=@timestampArray
             AND   ColJsonArray=@jsonArray
             AND   ColUuidArray=@uuidArray`
	_ = server.TestSpanner.PutStatementResult(
		query,
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateResultSetWithAllTypes(false, true),
		},
	)

	stmt, err := db.Prepare(query)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(stmt)
	ts, _ := time.Parse(time.RFC3339Nano, "2021-07-22T10:26:17.123Z")
	ts1, _ := time.Parse(time.RFC3339Nano, "2021-07-21T21:07:59.339911800Z")
	ts2, _ := time.Parse(time.RFC3339Nano, "2021-07-27T21:07:59.339911800Z")
	rows, err := stmt.QueryContext(
		context.Background(),
		true,
		"test",
		[]byte("testbytes"),
		uint(5),
		float32(3.14),
		3.14,
		numeric("6.626"),
		civil.Date{Year: 2021, Month: 7, Day: 21},
		ts,
		nullJson(true, `{"key":"value","other-key":["value1","value2"]}`),
		uuid.MustParse("a4e71944-fe14-4047-9d0a-e68c281602e1"),
		[]spanner.NullBool{{Valid: true, Bool: true}, {}, {Valid: true, Bool: false}},
		[]spanner.NullString{{Valid: true, StringVal: "test1"}, {}, {Valid: true, StringVal: "test2"}},
		[][]byte{[]byte("testbytes1"), nil, []byte("testbytes2")},
		[]spanner.NullInt64{{Valid: true, Int64: 1}, {}, {Valid: true, Int64: 2}},
		[]spanner.NullFloat32{{Valid: true, Float32: 3.14}, {}, {Valid: true, Float32: -99.99}},
		[]spanner.NullFloat64{{Valid: true, Float64: 6.626}, {}, {Valid: true, Float64: 10.01}},
		[]spanner.NullNumeric{nullNumeric(true, "3.14"), {}, nullNumeric(true, "10.01")},
		[]spanner.NullDate{{Valid: true, Date: civil.Date{Year: 2000, Month: 2, Day: 29}}, {}, {Valid: true, Date: civil.Date{Year: 2021, Month: 7, Day: 27}}},
		[]spanner.NullTime{{Valid: true, Time: ts1}, {}, {Valid: true, Time: ts2}},
		[]spanner.NullJSON{
			nullJson(true, `{"key1": "value1", "other-key1": ["value1", "value2"]}`),
			nullJson(false, ""),
			nullJson(true, `{"key2": "value2", "other-key2": ["value1", "value2"]}`),
		},
		[]spanner.NullUUID{
			nullUuid(true, `d0546638-6d51-4d7c-a4a9-9062204ee5bb`),
			nullUuid(false, ""),
			nullUuid(true, `0dd0f9b7-05af-48e0-a5b1-35432a01c6bf`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for rows.Next() {
		var b bool
		var s string
		var bt []byte
		var i int64
		var f32 float32
		var f float64
		var r big.Rat
		var d civil.Date
		var ts time.Time
		var j spanner.NullJSON
		var u uuid.UUID
		var p []byte
		var e int64
		var bArray []spanner.NullBool
		var sArray []spanner.NullString
		var btArray [][]byte
		var iArray []spanner.NullInt64
		var f32Array []spanner.NullFloat32
		var fArray []spanner.NullFloat64
		var rArray []spanner.NullNumeric
		var dArray []spanner.NullDate
		var tsArray []spanner.NullTime
		var jArray []spanner.NullJSON
		var uArray []spanner.NullUUID
		var pArray [][]byte
		var eArray []spanner.NullInt64
		err = rows.Scan(&b, &s, &bt, &i, &f32, &f, &r, &d, &ts, &j, &u, &p, &e, &bArray, &sArray, &btArray, &iArray, &f32Array, &fArray, &rArray, &dArray, &tsArray, &jArray, &uArray, &pArray, &eArray)
		if err != nil {
			t.Fatal(err)
		}
		if g, w := b, true; g != w {
			t.Errorf("row value mismatch for bool\nGot: %v\nWant: %v", g, w)
		}
		if g, w := s, "test"; g != w {
			t.Errorf("row value mismatch for string\nGot: %v\nWant: %v", g, w)
		}
		if g, w := bt, []byte("testbytes"); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bytes\nGot: %v\nWant: %v", g, w)
		}
		if g, w := i, int64(5); g != w {
			t.Errorf("row value mismatch for int64\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f32, float32(3.14); g != w {
			t.Errorf("row value mismatch for float32\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f, 3.14; g != w {
			t.Errorf("row value mismatch for float64\nGot: %v\nWant: %v", g, w)
		}
		if g, w := r, numeric("6.626"); g.Cmp(&w) != 0 {
			t.Errorf("row value mismatch for numeric\nGot: %v\nWant: %v", g, w)
		}
		if g, w := d, date("2021-07-21"); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for date\nGot: %v\nWant: %v", g, w)
		}
		if g, w := ts, time.Date(2021, 7, 21, 21, 7, 59, 339911800, time.UTC); g != w {
			t.Errorf("row value mismatch for timestamp\nGot: %v\nWant: %v", g, w)
		}
		if g, w := j, nullJson(true, `{"key":"value","other-key":["value1","value2"]}`); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json\nGot: %v\nWant: %v", g, w)
		}
		if g, w := u, uuid.MustParse(`a4e71944-fe14-4047-9d0a-e68c281602e1`); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for uuid\nGot: %v\nWant: %v", g, w)
		}
		wantSingerEnumValue := pb.Genre_ROCK
		wantSingerProtoMsg := pb.SingerInfo{
			SingerId:    proto.Int64(1),
			BirthDate:   proto.String("January"),
			Nationality: proto.String("Country1"),
			Genre:       &wantSingerEnumValue,
		}
		gotSingerProto := pb.SingerInfo{}
		if err := proto.Unmarshal(p, &gotSingerProto); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		if g, w := &gotSingerProto, &wantSingerProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
		if g, w := pb.Genre(e), wantSingerEnumValue; g != w {
			t.Errorf("row value mismatch for enum\nGot: %v\nWant: %v", g, w)
		}
		if g, w := bArray, []spanner.NullBool{{Valid: true, Bool: true}, {}, {Valid: true, Bool: false}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bool array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := sArray, []spanner.NullString{{Valid: true, StringVal: "test1"}, {}, {Valid: true, StringVal: "test2"}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for string array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := btArray, [][]byte{[]byte("testbytes1"), nil, []byte("testbytes2")}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bytes array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := iArray, []spanner.NullInt64{{Valid: true, Int64: 1}, {}, {Valid: true, Int64: 2}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for int array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f32Array, []spanner.NullFloat32{{Valid: true, Float32: 3.14}, {}, {Valid: true, Float32: -99.99}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for float32 array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := fArray, []spanner.NullFloat64{{Valid: true, Float64: 6.626}, {}, {Valid: true, Float64: 10.01}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for float64 array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := rArray, []spanner.NullNumeric{nullNumeric(true, "3.14"), {}, nullNumeric(true, "10.01")}; !cmp.Equal(g, w, cmp.AllowUnexported(big.Rat{}, big.Int{})) {
			t.Errorf("row value mismatch for numeric array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := dArray, []spanner.NullDate{{Valid: true, Date: civil.Date{Year: 2000, Month: 2, Day: 29}}, {}, {Valid: true, Date: civil.Date{Year: 2021, Month: 7, Day: 27}}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for date array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := tsArray, []spanner.NullTime{{Valid: true, Time: ts1}, {}, {Valid: true, Time: ts2}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for timestamp array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := jArray, []spanner.NullJSON{
			nullJson(true, `{"key1": "value1", "other-key1": ["value1", "value2"]}`),
			nullJson(false, ""),
			nullJson(true, `{"key2": "value2", "other-key2": ["value1", "value2"]}`),
		}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := uArray, []spanner.NullUUID{
			nullUuid(true, `d0546638-6d51-4d7c-a4a9-9062204ee5bb`),
			nullUuid(false, ""),
			nullUuid(true, `0dd0f9b7-05af-48e0-a5b1-35432a01c6bf`),
		}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := len(pArray), 3; g != w {
			t.Errorf("row value length mismatch for proto array\nGot: %v\nWant: %v", g, w)
		}
		wantSinger2ProtoEnum := pb.Genre_FOLK
		wantSinger2ProtoMsg := pb.SingerInfo{
			SingerId:    proto.Int64(2),
			BirthDate:   proto.String("February"),
			Nationality: proto.String("Country2"),
			Genre:       &wantSinger2ProtoEnum,
		}
		gotSingerProto1 := pb.SingerInfo{}
		if err := proto.Unmarshal(pArray[0], &gotSingerProto1); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		gotSingerProto2 := pb.SingerInfo{}
		if err := proto.Unmarshal(pArray[2], &gotSingerProto2); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		if g, w := &gotSingerProto1, &wantSingerProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
		if g, w := pArray[1], []byte(nil); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
		if g, w := &gotSingerProto2, &wantSinger2ProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.ParamTypes), 22; g != w {
		t.Fatalf("param types length mismatch\nGot: %v\nWant: %v", g, w)
	}
	if g, w := len(req.Params.Fields), 22; g != w {
		t.Fatalf("params length mismatch\nGot: %v\nWant: %v", g, w)
	}
	wantParams := []struct {
		name  string
		code  sppb.TypeCode
		array bool
		value interface{}
	}{
		{
			name:  "bool",
			code:  sppb.TypeCode_BOOL,
			value: true,
		},
		{
			name:  "string",
			code:  sppb.TypeCode_STRING,
			value: "test",
		},
		{
			name:  "bytes",
			code:  sppb.TypeCode_BYTES,
			value: base64.StdEncoding.EncodeToString([]byte("testbytes")),
		},
		{
			name:  "int64",
			code:  sppb.TypeCode_INT64,
			value: "5",
		},
		{
			name:  "float32",
			code:  sppb.TypeCode_FLOAT32,
			value: float64(float32(3.14)),
		},
		{
			name:  "float64",
			code:  sppb.TypeCode_FLOAT64,
			value: 3.14,
		},
		{
			name:  "numeric",
			code:  sppb.TypeCode_NUMERIC,
			value: "6.626000000",
		},
		{
			name:  "date",
			code:  sppb.TypeCode_DATE,
			value: "2021-07-21",
		},
		{
			name:  "timestamp",
			code:  sppb.TypeCode_TIMESTAMP,
			value: "2021-07-22T10:26:17.123Z",
		},
		{
			name:  "json",
			code:  sppb.TypeCode_JSON,
			value: `{"key":"value","other-key":["value1","value2"]}`,
		},
		{
			name:  "uuid",
			code:  sppb.TypeCode_UUID,
			value: `a4e71944-fe14-4047-9d0a-e68c281602e1`,
		},
		{
			name:  "boolArray",
			code:  sppb.TypeCode_BOOL,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_BoolValue{BoolValue: true}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_BoolValue{BoolValue: false}},
			}},
		},
		{
			name:  "stringArray",
			code:  sppb.TypeCode_STRING,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "test1"}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: "test2"}},
			}},
		},
		{
			name:  "bytesArray",
			code:  sppb.TypeCode_BYTES,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: base64.StdEncoding.EncodeToString([]byte("testbytes1"))}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: base64.StdEncoding.EncodeToString([]byte("testbytes2"))}},
			}},
		},
		{
			name:  "int64Array",
			code:  sppb.TypeCode_INT64,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "1"}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: "2"}},
			}},
		},
		{
			name:  "float32Array",
			code:  sppb.TypeCode_FLOAT32,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_NumberValue{NumberValue: float64(float32(3.14))}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_NumberValue{NumberValue: float64(float32(-99.99))}},
			}},
		},
		{
			name:  "float64Array",
			code:  sppb.TypeCode_FLOAT64,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_NumberValue{NumberValue: 6.626}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_NumberValue{NumberValue: 10.01}},
			}},
		},
		{
			name:  "numericArray",
			code:  sppb.TypeCode_NUMERIC,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "3.140000000"}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: "10.010000000"}},
			}},
		},
		{
			name:  "dateArray",
			code:  sppb.TypeCode_DATE,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "2000-02-29"}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-27"}},
			}},
		},
		{
			name:  "timestampArray",
			code:  sppb.TypeCode_TIMESTAMP,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-21T21:07:59.3399118Z"}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-27T21:07:59.3399118Z"}},
			}},
		},
		{
			name:  "jsonArray",
			code:  sppb.TypeCode_JSON,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: `{"key1":"value1","other-key1":["value1","value2"]}`}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: `{"key2":"value2","other-key2":["value1","value2"]}`}},
			}},
		},
		{
			name:  "uuidArray",
			code:  sppb.TypeCode_UUID,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: `d0546638-6d51-4d7c-a4a9-9062204ee5bb`}},
				{Kind: &structpb.Value_NullValue{}},
				{Kind: &structpb.Value_StringValue{StringValue: `0dd0f9b7-05af-48e0-a5b1-35432a01c6bf`}},
			}},
		},
	}
	for _, wantParam := range wantParams {
		if pt, ok := req.ParamTypes[wantParam.name]; ok {
			if wantParam.array {
				if g, w := pt.Code, sppb.TypeCode_ARRAY; g != w {
					t.Errorf("param type mismatch\nGot: %v\nWant: %v", g, w)
				}
				if g, w := pt.ArrayElementType.Code, wantParam.code; g != w {
					t.Errorf("param array element type mismatch\nGot: %v\nWant: %v", g, w)
				}
			} else {
				if g, w := pt.Code, wantParam.code; g != w {
					t.Errorf("param type mismatch\nGot: %v\nWant: %v", g, w)
				}
			}
		} else {
			t.Errorf("no param type found for @%s", wantParam.name)
		}
		if val, ok := req.Params.Fields[wantParam.name]; ok {
			var g interface{}
			if wantParam.array {
				g = val.GetListValue()
			} else {
				switch wantParam.code {
				case sppb.TypeCode_BOOL:
					g = val.GetBoolValue()
				case sppb.TypeCode_FLOAT32:
					g = val.GetNumberValue()
				case sppb.TypeCode_FLOAT64:
					g = val.GetNumberValue()
				default:
					g = val.GetStringValue()
				}
			}
			if wantParam.array {
				if !cmp.Equal(g, wantParam.value, cmpopts.IgnoreUnexported(structpb.ListValue{}, structpb.Value{})) {
					t.Errorf("array param value mismatch\nGot:  %v\nWant: %v", g, wantParam.value)
				}
			} else {
				if g != wantParam.value {
					t.Errorf("param value mismatch\nGot: %v\nWant: %v", g, wantParam.value)
				}
			}
		} else {
			t.Errorf("no value found for param @%s", wantParam.name)
		}
	}
}

func TestQueryWithNullParameters(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	query := `SELECT *
             FROM Test
             WHERE ColBool=@bool 
             AND   ColString=@string
             AND   ColBytes=@bytes
             AND   ColInt=@int64
             AND   ColFloat32=@float32
             AND   ColFloat64=@float64
             AND   ColNumeric=@numeric
             AND   ColDate=@date
             AND   ColTimestamp=@timestamp
             AND   ColJson=@json
             AND   ColUuid=@uuid
             AND   ColBoolArray=@boolArray
             AND   ColStringArray=@stringArray
             AND   ColBytesArray=@bytesArray
             AND   ColIntArray=@int64Array
             AND   ColFloat32Array=@float32Array
             AND   ColFloat64Array=@float64Array
             AND   ColNumericArray=@numericArray
             AND   ColDateArray=@dateArray
             AND   ColTimestampArray=@timestampArray
             AND   ColJsonArray=@jsonArray
             AND   ColUuidArray=@uuidArray`
	_ = server.TestSpanner.PutStatementResult(
		query,
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateResultSetWithAllTypes(true, true),
		},
	)

	stmt, err := db.Prepare(query)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(stmt)
	for _, p := range []struct {
		typed  int
		values []interface{}
	}{
		{
			typed: 0,
			values: []interface{}{
				nil, // bool
				nil, // string
				nil, // bytes
				nil, // int64
				nil, // float32
				nil, // float64
				nil, // numeric
				nil, // date
				nil, // timestamp
				nil, // json
				nil, // uuid
				nil, // bool array
				nil, // string array
				nil, // bytes array
				nil, // int64 array
				nil, // float32 array
				nil, // float64 array
				nil, // numeric array
				nil, // date array
				nil, // timestamp array
				nil, // json array
				nil, // uuid array
			}},
		{
			typed: 10,
			values: []interface{}{
				spanner.NullBool{},
				spanner.NullString{},
				nil, // bytes
				spanner.NullInt64{},
				spanner.NullFloat32{},
				spanner.NullFloat64{},
				spanner.NullNumeric{},
				spanner.NullDate{},
				spanner.NullTime{},
				spanner.NullJSON{},
				spanner.NullUUID{},
				nil, // bool array
				nil, // string array
				nil, // bytes array
				nil, // int64 array
				nil, // float32 array
				nil, // float64 array
				nil, // numeric array
				nil, // date array
				nil, // timestamp array
				nil, // json array
				nil, // uuid array
			}},
	} {
		rows, err := stmt.QueryContext(context.Background(), p.values...)
		if err != nil {
			t.Fatal(err)
		}
		defer silentClose(rows)

		for rows.Next() {
			var b sql.NullBool
			var s sql.NullString
			var bt []byte
			var i sql.NullInt64
			var f32 spanner.NullFloat32 // There's no equivalent sql type.
			var f sql.NullFloat64
			var r spanner.NullNumeric // There's no equivalent sql type.
			var d spanner.NullDate    // There's no equivalent sql type.
			var ts sql.NullTime
			var j spanner.NullJSON // There's no equivalent sql type.
			var u spanner.NullUUID // There's no equivalent sql type.
			var p []byte           // Proto columns are returned as bytes.
			var e sql.NullInt64    // Enum columns are returned as int64.
			var bArray []spanner.NullBool
			var sArray []spanner.NullString
			var btArray [][]byte
			var iArray []spanner.NullInt64
			var f32Array []spanner.NullFloat32
			var fArray []spanner.NullFloat64
			var rArray []spanner.NullNumeric
			var dArray []spanner.NullDate
			var tsArray []spanner.NullTime
			var jArray []spanner.NullJSON
			var uArray []spanner.NullUUID
			var pArray [][]byte
			var eArray []spanner.NullInt64
			err = rows.Scan(&b, &s, &bt, &i, &f32, &f, &r, &d, &ts, &j, &u, &p, &e, &bArray, &sArray, &btArray, &iArray, &f32Array, &fArray, &rArray, &dArray, &tsArray, &jArray, &uArray, &pArray, &eArray)
			if err != nil {
				t.Fatal(err)
			}
			if b.Valid {
				t.Errorf("row value mismatch for bool\nGot: %v\nWant: %v", b, spanner.NullBool{})
			}
			if s.Valid {
				t.Errorf("row value mismatch for string\nGot: %v\nWant: %v", s, spanner.NullString{})
			}
			if bt != nil {
				t.Errorf("row value mismatch for bytes\nGot: %v\nWant: %v", bt, nil)
			}
			if i.Valid {
				t.Errorf("row value mismatch for int64\nGot: %v\nWant: %v", i, spanner.NullInt64{})
			}
			if f32.Valid {
				t.Errorf("row value mismatch for float32\nGot: %v\nWant: %v", f, spanner.NullFloat32{})
			}
			if f.Valid {
				t.Errorf("row value mismatch for float64\nGot: %v\nWant: %v", f, spanner.NullFloat64{})
			}
			if r.Valid {
				t.Errorf("row value mismatch for numeric\nGot: %v\nWant: %v", r, spanner.NullNumeric{})
			}
			if d.Valid {
				t.Errorf("row value mismatch for date\nGot: %v\nWant: %v", d, spanner.NullDate{})
			}
			if ts.Valid {
				t.Errorf("row value mismatch for timestamp\nGot: %v\nWant: %v", ts, spanner.NullTime{})
			}
			if j.Valid {
				t.Errorf("row value mismatch for json\nGot: %v\nWant: %v", j, spanner.NullJSON{})
			}
			if u.Valid {
				t.Errorf("row value mismatch for uuid\n Got: %v\nWant: %v", u, spanner.NullUUID{})
			}
			if p != nil {
				t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", p, nil)
			}
			if e.Valid {
				t.Errorf("row value mismatch for enum\nGot: %v\nWant: %v", e, spanner.NullInt64{})
			}
			if bArray != nil {
				t.Errorf("row value mismatch for bool array\nGot: %v\nWant: %v", bArray, nil)
			}
			if sArray != nil {
				t.Errorf("row value mismatch for string array\nGot: %v\nWant: %v", sArray, nil)
			}
			if btArray != nil {
				t.Errorf("row value mismatch for bytes array array\nGot: %v\nWant: %v", btArray, nil)
			}
			if iArray != nil {
				t.Errorf("row value mismatch for int64 array\nGot: %v\nWant: %v", iArray, nil)
			}
			if f32Array != nil {
				t.Errorf("row value mismatch for float32 array\nGot: %v\nWant: %v", f32Array, nil)
			}
			if fArray != nil {
				t.Errorf("row value mismatch for float64 array\nGot: %v\nWant: %v", fArray, nil)
			}
			if rArray != nil {
				t.Errorf("row value mismatch for numeric array\nGot: %v\nWant: %v", rArray, nil)
			}
			if dArray != nil {
				t.Errorf("row value mismatch for date array\nGot: %v\nWant: %v", dArray, nil)
			}
			if tsArray != nil {
				t.Errorf("row value mismatch for timestamp array\nGot: %v\nWant: %v", tsArray, nil)
			}
			if jArray != nil {
				t.Errorf("row value mismatch for json array\nGot: %v\nWant: %v", jArray, nil)
			}
			if uArray != nil {
				t.Errorf("row value mismatch for uuid array\n Got: %v\nWant: %v", uArray, nil)
			}
			if pArray != nil {
				t.Errorf("row value mismatch for proto array\nGot: %v\nWant: %v", pArray, nil)
			}
			if eArray != nil {
				t.Errorf("row value mismatch for enum array\nGot: %v\nWant: %v", eArray, nil)
			}
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		requests := drainRequestsFromServer(server.TestSpanner)
		sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
		if g, w := len(sqlRequests), 1; g != w {
			t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
		}
		req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
		// The param types map should be empty when we are only sending untyped nil params.
		if g, w := len(req.ParamTypes), p.typed; g != w {
			t.Fatalf("param types length mismatch\nGot: %v\nWant: %v", g, w)
		}
		if g, w := len(req.Params.Fields), 22; g != w {
			t.Fatalf("params length mismatch\nGot: %v\nWant: %v", g, w)
		}
		for _, param := range req.Params.Fields {
			if _, ok := param.GetKind().(*structpb.Value_NullValue); !ok {
				t.Errorf("param value mismatch\nGot: %v\nWant: %v", param.GetKind(), structpb.Value_NullValue{})
			}
		}
	}
}

func TestQueryWithAllTypes_ReturnProto(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	query := "SELECT * FROM Test"
	_ = server.TestSpanner.PutStatementResult(
		query,
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateResultSetWithAllTypes(false, true),
		},
	)

	for _, prepare := range []bool{false, true} {
		var rows *sql.Rows
		if prepare {
			stmt, err := db.Prepare(query)
			if err != nil {
				t.Fatal(err)
			}
			rows, err = stmt.QueryContext(context.Background(), ExecOptions{DecodeOption: DecodeOptionProto})
			if err != nil {
				t.Fatal(err)
			}
			_ = stmt.Close()
		} else {
			var err error
			rows, err = db.QueryContext(context.Background(), query, ExecOptions{DecodeOption: DecodeOptionProto})
			if err != nil {
				t.Fatal(err)
			}
		}

		for rows.Next() {
			var b spanner.GenericColumnValue
			var s spanner.GenericColumnValue
			var bt spanner.GenericColumnValue
			var i spanner.GenericColumnValue
			var f32 spanner.GenericColumnValue
			var f spanner.GenericColumnValue
			var r spanner.GenericColumnValue
			var d spanner.GenericColumnValue
			var ts spanner.GenericColumnValue
			var j spanner.GenericColumnValue
			var u spanner.GenericColumnValue
			var p spanner.GenericColumnValue
			var e spanner.GenericColumnValue
			var bArray spanner.GenericColumnValue
			var sArray spanner.GenericColumnValue
			var btArray spanner.GenericColumnValue
			var iArray spanner.GenericColumnValue
			var f32Array spanner.GenericColumnValue
			var fArray spanner.GenericColumnValue
			var rArray spanner.GenericColumnValue
			var dArray spanner.GenericColumnValue
			var tsArray spanner.GenericColumnValue
			var jArray spanner.GenericColumnValue
			var uArray spanner.GenericColumnValue
			var pArray spanner.GenericColumnValue
			var eArray spanner.GenericColumnValue
			err := rows.Scan(&b, &s, &bt, &i, &f32, &f, &r, &d, &ts, &j, &u, &p, &e, &bArray, &sArray, &btArray, &iArray, &f32Array, &fArray, &rArray, &dArray, &tsArray, &jArray, &uArray, &pArray, &eArray)
			if err != nil {
				t.Fatalf("prepare: %v: failed to scan values: %v", prepare, err)
			}
			if g, w := b.Value.GetBoolValue(), true; g != w {
				t.Errorf("row value mismatch for bool\nGot: %v\nWant: %v", g, w)
			}
			if g, w := s.Value.GetStringValue(), "test"; g != w {
				t.Errorf("row value mismatch for string\nGot: %v\nWant: %v", g, w)
			}
			if g, w := bt.Value.GetStringValue(), base64.RawURLEncoding.EncodeToString([]byte("testbytes")); !cmp.Equal(g, w) {
				t.Errorf("row value mismatch for bytes\nGot: %v\nWant: %v", g, w)
			}
			if g, w := i.Value.GetStringValue(), "5"; g != w {
				t.Errorf("row value mismatch for int64\nGot: %v\nWant: %v", g, w)
			}
			if g, w := float32(f32.Value.GetNumberValue()), float32(3.14); g != w {
				t.Errorf("row value mismatch for float32\nGot: %v\nWant: %v", g, w)
			}
			if g, w := f.Value.GetNumberValue(), 3.14; g != w {
				t.Errorf("row value mismatch for float64\nGot: %v\nWant: %v", g, w)
			}
			if g, w := r.Value.GetStringValue(), "6.626"; g != w {
				t.Errorf("row value mismatch for numeric\nGot: %v\nWant: %v", g, w)
			}
			if g, w := d.Value.GetStringValue(), "2021-07-21"; g != w {
				t.Errorf("row value mismatch for date\nGot: %v\nWant: %v", g, w)
			}
			if g, w := ts.Value.GetStringValue(), "2021-07-21T21:07:59.339911800Z"; g != w {
				t.Errorf("row value mismatch for timestamp\nGot: %v\nWant: %v", g, w)
			}
			if g, w := j.Value.GetStringValue(), `{"key": "value", "other-key": ["value1", "value2"]}`; g != w {
				t.Errorf("row value mismatch for json\n Got: %v\nWant: %v", g, w)
			}
			if g, w := u.Value.GetStringValue(), `a4e71944-fe14-4047-9d0a-e68c281602e1`; g != w {
				t.Errorf("row value mismatch for uuid\n Got: %v\nWant: %v", g, w)
			}
			if p.Value.GetStringValue() == "" {
				t.Errorf("row value mismatch for proto\n Got: %v\nWant: A non-empty string", p.Value.GetStringValue())
			}
			if g, w := e.Value.GetStringValue(), "3"; g != w {
				t.Errorf("row value mismatch for enum\n Got: %v\nWant: %v", g, w)
			}
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		_ = rows.Close()
	}
}

func TestQueryWithAllNativeTypes(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnectionWithParams(t, "DecodeToNativeArrays=true")
	defer teardown()
	query := `SELECT *
             FROM Test
             WHERE ColBool=@bool 
             AND   ColString=@string
             AND   ColBytes=@bytes
             AND   ColInt=@int64
             AND   ColFloat32=@float32
             AND   ColFloat64=@float64
             AND   ColNumeric=@numeric
             AND   ColDate=@date
             AND   ColTimestamp=@timestamp
             AND   ColJson=@json
             AND   ColUuid=@uuid
             AND   ColBoolArray=@boolArray
             AND   ColStringArray=@stringArray
             AND   ColBytesArray=@bytesArray
             AND   ColIntArray=@int64Array
             AND   ColFloat32Array=@float32Array
             AND   ColFloat64Array=@float64Array
             AND   ColNumericArray=@numericArray
             AND   ColDateArray=@dateArray
             AND   ColTimestampArray=@timestampArray
             AND   ColJsonArray=@jsonArray
             AND   ColUuidArray=@uuidArray`
	_ = server.TestSpanner.PutStatementResult(
		query,
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateResultSetWithAllTypes(false, false),
		},
	)

	stmt, err := db.Prepare(query)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(stmt)
	ts, _ := time.Parse(time.RFC3339Nano, "2021-07-22T10:26:17.123Z")
	ts1, _ := time.Parse(time.RFC3339Nano, "2021-07-21T21:07:59.339911800Z")
	ts2, _ := time.Parse(time.RFC3339Nano, "2021-07-27T21:07:59.339911800Z")
	tsAlt, _ := time.Parse(time.RFC3339Nano, "2000-01-01T00:00:00Z")
	rows, err := stmt.QueryContext(
		context.Background(),
		true,
		"test",
		[]byte("testbytes"),
		uint(5),
		float32(3.14),
		3.14,
		numeric("6.626"),
		civil.Date{Year: 2021, Month: 7, Day: 21},
		ts,
		nullJson(true, `{"key":"value","other-key":["value1","value2"]}`),
		uuid.MustParse("a4e71944-fe14-4047-9d0a-e68c281602e1"),
		[]bool{true, false},
		[]string{"test1", "test2"},
		[][]byte{[]byte("testbytes1"), []byte("testbytes2")},
		[]int64{1, 2},
		[]float32{3.14, -99.99},
		[]float64{6.626, 10.01},
		[]spanner.NullNumeric{nullNumeric(true, "3.14"), nullNumeric(true, "10.01")},
		[]civil.Date{{Year: 2000, Month: 2, Day: 29}, {Year: 2021, Month: 7, Day: 27}},
		[]time.Time{ts1, ts2},
		[]spanner.NullJSON{
			nullJson(true, `{"key1": "value1", "other-key1": ["value1", "value2"]}`),
			nullJson(true, `{"key2": "value2", "other-key2": ["value1", "value2"]}`),
		},
		[]uuid.UUID{
			uuid.MustParse("d0546638-6d51-4d7c-a4a9-9062204ee5bb"),
			uuid.MustParse("0dd0f9b7-05af-48e0-a5b1-35432a01c6bf"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)

	for rows.Next() {
		var b bool
		var s string
		var bt []byte
		var i int64
		var f32 float32
		var f float64
		var r big.Rat
		var d civil.Date
		var ts time.Time
		var j spanner.NullJSON
		var u uuid.UUID
		var p []byte
		var e int64
		var bArray []bool
		var sArray []string
		var btArray [][]byte
		var iArray []int64
		var f32Array []float32
		var fArray []float64
		var rArray []spanner.NullNumeric
		var dArray []civil.Date
		var tsArray []time.Time
		var jArray []spanner.NullJSON
		var uArray []uuid.UUID
		var pArray [][]byte
		var eArray []int64
		err = rows.Scan(&b, &s, &bt, &i, &f32, &f, &r, &d, &ts, &j, &u, &p, &e, &bArray, &sArray, &btArray, &iArray, &f32Array, &fArray, &rArray, &dArray, &tsArray, &jArray, &uArray, &pArray, &eArray)
		if err != nil {
			t.Fatal(err)
		}
		if g, w := b, true; g != w {
			t.Errorf("row value mismatch for bool\nGot: %v\nWant: %v", g, w)
		}
		if g, w := s, "test"; g != w {
			t.Errorf("row value mismatch for string\nGot: %v\nWant: %v", g, w)
		}
		if g, w := bt, []byte("testbytes"); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bytes\nGot: %v\nWant: %v", g, w)
		}
		if g, w := i, int64(5); g != w {
			t.Errorf("row value mismatch for int64\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f32, float32(3.14); g != w {
			t.Errorf("row value mismatch for float32\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f, 3.14; g != w {
			t.Errorf("row value mismatch for float64\nGot: %v\nWant: %v", g, w)
		}
		if g, w := r, numeric("6.626"); g.Cmp(&w) != 0 {
			t.Errorf("row value mismatch for numeric\nGot: %v\nWant: %v", g, w)
		}
		if g, w := d, date("2021-07-21"); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for date\nGot: %v\nWant: %v", g, w)
		}
		if g, w := ts, time.Date(2021, 7, 21, 21, 7, 59, 339911800, time.UTC); g != w {
			t.Errorf("row value mismatch for timestamp\nGot: %v\nWant: %v", g, w)
		}
		if g, w := j, nullJson(true, `{"key":"value","other-key":["value1","value2"]}`); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json\nGot: %v\nWant: %v", g, w)
		}
		if g, w := u, uuid.MustParse("a4e71944-fe14-4047-9d0a-e68c281602e1"); !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for uuid\n Got: %v\nWant: %v", g, w)
		}
		wantSingerEnumValue := pb.Genre_ROCK
		wantSingerProtoMsg := pb.SingerInfo{
			SingerId:    proto.Int64(1),
			BirthDate:   proto.String("January"),
			Nationality: proto.String("Country1"),
			Genre:       &wantSingerEnumValue,
		}
		gotSingerProto := pb.SingerInfo{}
		if err := proto.Unmarshal(p, &gotSingerProto); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		if g, w := &gotSingerProto, &wantSingerProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
		if g, w := pb.Genre(e), wantSingerEnumValue; g != w {
			t.Errorf("row value mismatch for enum\nGot: %v\nWant: %v", g, w)
		}
		if g, w := bArray, []bool{true, true, false}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bool array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := sArray, []string{"test1", "alt", "test2"}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for string array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := btArray, [][]byte{[]byte("testbytes1"), []byte("altbytes"), []byte("testbytes2")}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for bytes array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := iArray, []int64{1, 0, 2}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for int array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := f32Array, []float32{3.14, 0.0, -99.99}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for float32 array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := fArray, []float64{6.626, 0.0, 10.01}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for float64 array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := rArray, []spanner.NullNumeric{nullNumeric(true, "3.14"), nullNumeric(true, "1.0"), nullNumeric(true, "10.01")}; !cmp.Equal(g, w, cmp.AllowUnexported(big.Rat{}, big.Int{})) {
			t.Errorf("row value mismatch for numeric array\n Got: %v\nWant: %v", g, w)
		}
		if g, w := dArray, []civil.Date{{Year: 2000, Month: 2, Day: 29}, {Year: 2000, Month: 1, Day: 1}, {Year: 2021, Month: 7, Day: 27}}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for date array\nGot: %v\nWant: %v", g, w)
		}
		if g, w := tsArray, []time.Time{ts1, tsAlt, ts2}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for timestamp array\n Got: %v\nWant: %v", g, w)
		}
		if g, w := jArray, []spanner.NullJSON{
			nullJson(true, `{"key1": "value1", "other-key1": ["value1", "value2"]}`),
			nullJson(true, "{}"),
			nullJson(true, `{"key2": "value2", "other-key2": ["value1", "value2"]}`),
		}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json array\n Got: %v\nWant: %v", g, w)
		}
		if g, w := uArray, []uuid.UUID{
			uuid.MustParse("d0546638-6d51-4d7c-a4a9-9062204ee5bb"),
			uuid.MustParse("00000000-0000-0000-0000-000000000000"),
			uuid.MustParse("0dd0f9b7-05af-48e0-a5b1-35432a01c6bf"),
		}; !cmp.Equal(g, w) {
			t.Errorf("row value mismatch for json array\n Got: %v\nWant: %v", g, w)
		}
		if g, w := len(pArray), 3; g != w {
			t.Errorf("row value length mismatch for proto array\nGot: %v\nWant: %v", g, w)
		}
		wantSinger2ProtoEnum := pb.Genre_FOLK
		wantSinger2ProtoMsg := pb.SingerInfo{
			SingerId:    proto.Int64(2),
			BirthDate:   proto.String("February"),
			Nationality: proto.String("Country2"),
			Genre:       &wantSinger2ProtoEnum,
		}
		gotSingerProto1 := pb.SingerInfo{}
		if err := proto.Unmarshal(pArray[0], &gotSingerProto1); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		gotSingerProtoAlt := pb.SingerInfo{}
		if err := proto.Unmarshal(pArray[1], &gotSingerProtoAlt); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		gotSingerProto2 := pb.SingerInfo{}
		if err := proto.Unmarshal(pArray[2], &gotSingerProto2); err != nil {
			t.Fatalf("failed to unmarshal proto: %v", err)
		}
		if g, w := &gotSingerProto1, &wantSingerProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
		if g, w := &gotSingerProtoAlt, &wantSingerProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\n Got: %v\nWant: %v", g, w)
		}
		if g, w := &gotSingerProto2, &wantSinger2ProtoMsg; !cmp.Equal(g, w, cmpopts.IgnoreUnexported(pb.SingerInfo{})) {
			t.Errorf("row value mismatch for proto\nGot: %v\nWant: %v", g, w)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.ParamTypes), 22; g != w {
		t.Fatalf("param types length mismatch\nGot: %v\nWant: %v", g, w)
	}
	if g, w := len(req.Params.Fields), 22; g != w {
		t.Fatalf("params length mismatch\nGot: %v\nWant: %v", g, w)
	}
	wantParams := []struct {
		name  string
		code  sppb.TypeCode
		array bool
		value interface{}
	}{
		{
			name:  "bool",
			code:  sppb.TypeCode_BOOL,
			value: true,
		},
		{
			name:  "string",
			code:  sppb.TypeCode_STRING,
			value: "test",
		},
		{
			name:  "bytes",
			code:  sppb.TypeCode_BYTES,
			value: base64.StdEncoding.EncodeToString([]byte("testbytes")),
		},
		{
			name:  "int64",
			code:  sppb.TypeCode_INT64,
			value: "5",
		},
		{
			name:  "float32",
			code:  sppb.TypeCode_FLOAT32,
			value: float64(float32(3.14)),
		},
		{
			name:  "float64",
			code:  sppb.TypeCode_FLOAT64,
			value: 3.14,
		},
		{
			name:  "numeric",
			code:  sppb.TypeCode_NUMERIC,
			value: "6.626000000",
		},
		{
			name:  "date",
			code:  sppb.TypeCode_DATE,
			value: "2021-07-21",
		},
		{
			name:  "timestamp",
			code:  sppb.TypeCode_TIMESTAMP,
			value: "2021-07-22T10:26:17.123Z",
		},
		{
			name:  "json",
			code:  sppb.TypeCode_JSON,
			value: `{"key":"value","other-key":["value1","value2"]}`,
		},
		{
			name:  "uuid",
			code:  sppb.TypeCode_UUID,
			value: `a4e71944-fe14-4047-9d0a-e68c281602e1`,
		},
		{
			name:  "boolArray",
			code:  sppb.TypeCode_BOOL,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_BoolValue{BoolValue: true}},
				{Kind: &structpb.Value_BoolValue{BoolValue: false}},
			}},
		},
		{
			name:  "stringArray",
			code:  sppb.TypeCode_STRING,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "test1"}},
				{Kind: &structpb.Value_StringValue{StringValue: "test2"}},
			}},
		},
		{
			name:  "bytesArray",
			code:  sppb.TypeCode_BYTES,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: base64.StdEncoding.EncodeToString([]byte("testbytes1"))}},
				{Kind: &structpb.Value_StringValue{StringValue: base64.StdEncoding.EncodeToString([]byte("testbytes2"))}},
			}},
		},
		{
			name:  "int64Array",
			code:  sppb.TypeCode_INT64,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "1"}},
				{Kind: &structpb.Value_StringValue{StringValue: "2"}},
			}},
		},
		{
			name:  "float32Array",
			code:  sppb.TypeCode_FLOAT32,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_NumberValue{NumberValue: float64(float32(3.14))}},
				{Kind: &structpb.Value_NumberValue{NumberValue: float64(float32(-99.99))}},
			}},
		},
		{
			name:  "float64Array",
			code:  sppb.TypeCode_FLOAT64,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_NumberValue{NumberValue: 6.626}},
				{Kind: &structpb.Value_NumberValue{NumberValue: 10.01}},
			}},
		},
		{
			name:  "numericArray",
			code:  sppb.TypeCode_NUMERIC,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "3.140000000"}},
				{Kind: &structpb.Value_StringValue{StringValue: "10.010000000"}},
			}},
		},
		{
			name:  "dateArray",
			code:  sppb.TypeCode_DATE,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "2000-02-29"}},
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-27"}},
			}},
		},
		{
			name:  "timestampArray",
			code:  sppb.TypeCode_TIMESTAMP,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-21T21:07:59.3399118Z"}},
				{Kind: &structpb.Value_StringValue{StringValue: "2021-07-27T21:07:59.3399118Z"}},
			}},
		},
		{
			name:  "jsonArray",
			code:  sppb.TypeCode_JSON,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: `{"key1":"value1","other-key1":["value1","value2"]}`}},
				{Kind: &structpb.Value_StringValue{StringValue: `{"key2":"value2","other-key2":["value1","value2"]}`}},
			}},
		},
		{
			name:  "uuidArray",
			code:  sppb.TypeCode_UUID,
			array: true,
			value: &structpb.ListValue{Values: []*structpb.Value{
				{Kind: &structpb.Value_StringValue{StringValue: `d0546638-6d51-4d7c-a4a9-9062204ee5bb`}},
				{Kind: &structpb.Value_StringValue{StringValue: `0dd0f9b7-05af-48e0-a5b1-35432a01c6bf`}},
			}},
		},
	}
	for _, wantParam := range wantParams {
		if pt, ok := req.ParamTypes[wantParam.name]; ok {
			if wantParam.array {
				if g, w := pt.Code, sppb.TypeCode_ARRAY; g != w {
					t.Errorf("param type mismatch\nGot: %v\nWant: %v", g, w)
				}
				if g, w := pt.ArrayElementType.Code, wantParam.code; g != w {
					t.Errorf("param array element type mismatch\nGot: %v\nWant: %v", g, w)
				}
			} else {
				if g, w := pt.Code, wantParam.code; g != w {
					t.Errorf("param type mismatch\nGot: %v\nWant: %v", g, w)
				}
			}
		} else {
			t.Errorf("no param type found for @%s", wantParam.name)
		}
		if val, ok := req.Params.Fields[wantParam.name]; ok {
			var g interface{}
			if wantParam.array {
				g = val.GetListValue()
			} else {
				switch wantParam.code {
				case sppb.TypeCode_BOOL:
					g = val.GetBoolValue()
				case sppb.TypeCode_FLOAT32:
					g = val.GetNumberValue()
				case sppb.TypeCode_FLOAT64:
					g = val.GetNumberValue()
				default:
					g = val.GetStringValue()
				}
			}
			if wantParam.array {
				if !cmp.Equal(g, wantParam.value, cmpopts.IgnoreUnexported(structpb.ListValue{}, structpb.Value{})) {
					t.Errorf("array param value mismatch\nGot:  %v\nWant: %v", g, wantParam.value)
				}
			} else {
				if g != wantParam.value {
					t.Errorf("param value mismatch\nGot: %v\nWant: %v", g, wantParam.value)
				}
			}
		} else {
			t.Errorf("no value found for param @%s", wantParam.name)
		}
	}
}

func TestDmlInAutocommit(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(conn)
	_, err = conn.ExecContext(ctx, "set max_commit_delay=100")
	if err != nil {
		t.Fatal(err)
	}

	res, err := conn.ExecContext(ctx, testutil.UpdateBarSetFoo)
	if err != nil {
		t.Fatal(err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if g, w := affected, int64(testutil.UpdateBarSetFooRowCount); g != w {
		t.Fatalf("row count mismatch\nGot: %v\nWant: %v", g, w)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	// The DML statement should use a transaction even though no explicit
	// transaction was created.
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if _, ok := req.Transaction.Selector.(*sppb.TransactionSelector_Begin); !ok {
		t.Fatalf("unsupported transaction type %T", req.Transaction.Selector)
	}
	if !req.LastStatement {
		t.Fatal("missing LastStatement for ExecuteSqlRequest")
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitReq := commitRequests[0].(*sppb.CommitRequest)
	if commitReq.GetTransactionId() == nil {
		t.Fatalf("missing id selector for CommitRequest")
	}
	if g, w := commitReq.MaxCommitDelay.Nanos, int32(time.Millisecond*100); g != w {
		t.Fatalf("max_commit_delay mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestQueryWithDuplicateNamedParameter(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	s := "insert into users (id, name) values (@name, @name)"
	_ = server.TestSpanner.PutStatementResult(s, &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_, err := db.Exec(s, sql.Named("name", "foo"), sql.Named("name", "bar"))
	if err != nil {
		t.Fatal(err)
	}
	// Verify that 'bar' is used for both instances of the parameter @name.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := req.Params.Fields["name"].GetStringValue(), "bar"; g != w {
		t.Fatalf("param value mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestQueryWithReusedNamedParameter(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	s := "insert into users (id, name) values (@name, @name)"
	_ = server.TestSpanner.PutStatementResult(s, &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_, err := db.Exec(s, sql.Named("name", "foo"))
	if err != nil {
		t.Fatal(err)
	}
	// Verify that 'foo' is used for both instances of the parameter @name.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := req.Params.Fields["name"].GetStringValue(), "foo"; g != w {
		t.Fatalf("param value mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestQueryWithReusedPositionalParameter(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	s := "insert into users (id, name) values (@name, @name)"
	_ = server.TestSpanner.PutStatementResult(s, &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_, err := db.Exec(s, "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	// Verify that 'bar' is used for both instances of the parameter @name.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := req.Params.Fields["name"].GetStringValue(), "bar"; g != w {
		t.Fatalf("param value mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestQueryWithMissingPositionalParameter(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	s := "insert into users (id, name) values (@name, @name)"
	_ = server.TestSpanner.PutStatementResult(s, &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_, err := db.Exec(s, "foo")
	if err != nil {
		t.Fatal(err)
	}
	// Verify that 'foo' is used for the parameter @name.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := req.Params.Fields["name"].GetStringValue(), "foo"; g != w {
		t.Fatalf("param value mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestDmlReturningInAutocommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	s := "insert into users (id, name) values (@id, @name) then return id"
	_ = server.TestSpanner.PutStatementResult(
		s,
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateSelect1ResultSet(),
		},
	)

	for _, prepare := range []bool{false, true} {
		var rows *sql.Rows
		var err error
		if prepare {
			var stmt *sql.Stmt
			stmt, err = db.PrepareContext(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			rows, err = stmt.QueryContext(ctx, sql.Named("id", 1), sql.Named("name", "bar"))
		} else {
			rows, err = db.QueryContext(ctx, s, sql.Named("id", 1), sql.Named("name", "bar"))
		}
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatal("missing row")
		}
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if g, w := id, 1; g != w {
			t.Fatalf("id mismatch\n Got: %v\nWant: %v", g, w)
		}
		if rows.Next() {
			t.Fatal("got more rows than expected")
		}

		// Verify that a read/write transaction was used.
		requests := drainRequestsFromServer(server.TestSpanner)
		sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
		if g, w := len(sqlRequests), 1; g != w {
			t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
		}
		if !sqlRequests[0].(*sppb.ExecuteSqlRequest).LastStatement {
			t.Fatal("missing LastStatement for ExecuteSqlRequest")
		}
		commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
		if g, w := len(commitRequests), 1; g != w {
			t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
		}
	}
}

func TestDdlInAutocommit(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	var expectedResponse = &emptypb.Empty{}
	anyMsg, _ := anypb.New(expectedResponse)
	server.TestDatabaseAdmin.SetResps([]proto.Message{
		&longrunningpb.Operation{
			Done:   true,
			Result: &longrunningpb.Operation_Response{Response: anyMsg},
			Name:   "test-operation",
		},
	})
	query := "CREATE TABLE Singers (SingerId INT64, FirstName STRING(100), LastName STRING(100)) PRIMARY KEY (SingerId)"
	_, err := db.ExecContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	requests := server.TestDatabaseAdmin.Reqs()
	if g, w := len(requests), 1; g != w {
		t.Fatalf("requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	if req, ok := requests[0].(*databasepb.UpdateDatabaseDdlRequest); ok {
		if g, w := len(req.Statements), 1; g != w {
			t.Fatalf("statement count mismatch\nGot: %v\nWant: %v", g, w)
		}
		if g, w := req.Statements[0], query; g != w {
			t.Fatalf("statement mismatch\nGot: %v\nWant: %v", g, w)
		}
	} else {
		t.Fatalf("request type mismatch, got %v", requests[0])
	}
}

func TestDdlInTransaction(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	query := "CREATE TABLE Singers (SingerId INT64, FirstName STRING(100), LastName STRING(100)) PRIMARY KEY (SingerId)"
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), query); spanner.ErrCode(err) != codes.FailedPrecondition {
		t.Fatalf("error mismatch\nGot:  %v\nWant: %v", spanner.ErrCode(err), codes.FailedPrecondition)
	}
	requests := server.TestDatabaseAdmin.Reqs()
	if g, w := len(requests), 0; g != w {
		t.Fatalf("requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestBegin(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	// Ensure that the old Begin method works.
	_, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
}

func TestQuery(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	// Ensure that the old Query method works.
	rows, err := db.Query(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	defer silentClose(rows)
}

func TestExec(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	// Ensure that the old Exec method works.
	_, err := db.Exec(testutil.UpdateBarSetFoo)
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	// Ensure that the old Prepare method works.
	_, err := db.Prepare(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
}

func TestApplyMutations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get connection: %v", err)
	}
	var commitTimestamp time.Time
	if err := conn.Raw(func(driverConn interface{}) error {
		spannerConn, ok := driverConn.(SpannerConn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %v, expected SpannerConn", driverConn)
		}
		commitTimestamp, err = spannerConn.Apply(ctx, []*spanner.Mutation{
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(1), "Foo", int64(50)}),
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(2), "Bar", int64(1)}),
		})
		return err
	}); err != nil {
		t.Fatalf("failed to apply mutations: %v", err)
	}
	if commitTimestamp.Equal(time.Time{}) {
		t.Fatal("no commit timestamp returned")
	}

	// Even though the Apply method is used outside a transaction, the connection will internally start a read/write
	// transaction for the mutations.
	requests := drainRequestsFromServer(server.TestSpanner)
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := len(commitRequest.Mutations), 2; g != w {
		t.Fatalf("mutation count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestApplyMutationsFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	con, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get connection: %v", err)
	}
	_, err = con.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if g, w := spanner.ErrCode(con.Raw(func(driverConn interface{}) error {
		spannerConn, ok := driverConn.(SpannerConn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %v, expected SpannerConn", driverConn)
		}
		_, err = spannerConn.Apply(ctx, []*spanner.Mutation{
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(1), "Foo", int64(50)}),
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(2), "Bar", int64(1)}),
		})
		return err
	})), codes.FailedPrecondition; g != w {
		t.Fatalf("error code mismatch for Apply during transaction\nGot:  %v\nWant: %v", g, w)
	}
}

func TestBufferWriteMutations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	con, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get connection: %v", err)
	}
	tx, err := con.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	if err := con.Raw(func(driverConn interface{}) error {
		spannerConn, ok := driverConn.(SpannerConn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %v, expected SpannerConn", driverConn)
		}
		return spannerConn.BufferWrite([]*spanner.Mutation{
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(1), "Foo", int64(50)}),
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(2), "Bar", int64(1)}),
		})
	}); err != nil {
		t.Fatalf("failed to buffer mutations: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit transaction: %v", err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := len(commitRequest.Mutations), 2; g != w {
		t.Fatalf("mutation count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestBufferWriteMutationsFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	con, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get connection: %v", err)
	}
	if g, w := spanner.ErrCode(con.Raw(func(driverConn interface{}) error {
		spannerConn, ok := driverConn.(SpannerConn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %v, expected SpannerConn", driverConn)
		}
		return spannerConn.BufferWrite([]*spanner.Mutation{
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(1), "Foo", int64(50)}),
			spanner.Insert("Accounts", []string{"AccountId", "Nickname", "Balance"}, []interface{}{int64(2), "Bar", int64(1)}),
		})
	})), codes.FailedPrecondition; g != w {
		t.Fatalf("error code mismatch for BufferWrite outside transaction\nGot:  %v\nWant: %v", g, w)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	// Ensure that the old Ping method works.
	err := db.Ping()
	if err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

func TestDdlBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	var expectedResponse = &emptypb.Empty{}
	anyMsg, _ := anypb.New(expectedResponse)
	server.TestDatabaseAdmin.SetResps([]proto.Message{
		&longrunningpb.Operation{
			Done:   true,
			Result: &longrunningpb.Operation_Response{Response: anyMsg},
			Name:   "test-operation",
		},
	})

	statements := []string{"CREATE TABLE FOO", "CREATE TABLE BAR"}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(conn)

	if _, err = conn.ExecContext(ctx, "START BATCH DDL"); err != nil {
		t.Fatalf("failed to start DDL batch: %v", err)
	}
	for _, stmt := range statements {
		if _, err = conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("failed to execute statement in DDL batch: %v", err)
		}
	}
	if _, err = conn.ExecContext(ctx, "RUN BATCH"); err != nil {
		t.Fatalf("failed to run DDL batch: %v", err)
	}

	requests := server.TestDatabaseAdmin.Reqs()
	if g, w := len(requests), 1; g != w {
		t.Fatalf("requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	if req, ok := requests[0].(*databasepb.UpdateDatabaseDdlRequest); ok {
		if g, w := len(req.Statements), len(statements); g != w {
			t.Fatalf("statement count mismatch\nGot: %v\nWant: %v", g, w)
		}
		for i, stmt := range statements {
			if g, w := req.Statements[i], stmt; g != w {
				t.Fatalf("statement mismatch\nGot: %v\nWant: %v", g, w)
			}
		}
	} else {
		t.Fatalf("request type mismatch, got %v", requests[0])
	}
}

func TestAbortDdlBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	statements := []string{"CREATE TABLE FOO", "CREATE TABLE BAR"}
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(c)

	if _, err = c.ExecContext(ctx, "START BATCH DDL"); err != nil {
		t.Fatalf("failed to start DDL batch: %v", err)
	}
	for _, stmt := range statements {
		if _, err = c.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("failed to execute statement in DDL batch: %v", err)
		}
	}
	// Check that the statements have been batched.
	_ = c.Raw(func(driverConn interface{}) error {
		conn := driverConn.(*conn)
		if conn.batch == nil {
			t.Fatalf("missing batch on connection")
		}
		if g, w := len(conn.batch.statements), 2; g != w {
			t.Fatalf("batch length mismatch\nGot: %v\nWant: %v", g, w)
		}
		return nil
	})

	if _, err = c.ExecContext(ctx, "ABORT BATCH"); err != nil {
		t.Fatalf("failed to abort DDL batch: %v", err)
	}

	requests := server.TestDatabaseAdmin.Reqs()
	if g, w := len(requests), 0; g != w {
		t.Fatalf("requests count mismatch\nGot: %v\nWant: %v", g, w)
	}

	_ = c.Raw(func(driverConn interface{}) error {
		spannerConn := driverConn.(SpannerConn)
		if spannerConn.InDDLBatch() {
			t.Fatalf("connection still has an active DDL batch")
		}
		return nil
	})
}

func TestShowAndSetVariableRetryAbortsInternally(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to obtain a connection: %v", err)
	}
	defer silentClose(c)

	for _, tc := range []struct {
		expected bool
		set      bool
	}{
		{expected: true, set: false},
		{expected: false, set: true},
		{expected: true, set: true},
	} {
		// Get the current value.
		rows, err := c.QueryContext(ctx, "SHOW VARIABLE RETRY_ABORTS_INTERNALLY")
		if err != nil {
			t.Fatalf("failed to execute get variable retry_aborts_internally: %v", err)
		}
		defer silentClose(rows)
		for rows.Next() {
			var retry bool
			if err := rows.Scan(&retry); err != nil {
				t.Fatalf("failed to scan value for retry_aborts_internally: %v", err)
			}
			if g, w := retry, tc.expected; g != w {
				t.Fatalf("retry_aborts_internally mismatch\nGot: %v\nWant: %v", g, w)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("failed to iterate over result for get variable retry_aborts_internally: %v", err)
		}

		// Check that the behavior matches the setting.
		tx, _ := c.BeginTx(ctx, nil)
		server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
			Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
		})
		err = tx.Commit()
		if tc.expected && err != nil {
			t.Fatalf("unexpected error for commit: %v", err)
		} else if !tc.expected && spanner.ErrCode(err) != codes.Aborted {
			t.Fatalf("error code mismatch\nGot: %v\nWant: %v", spanner.ErrCode(err), codes.Aborted)
		}

		// Set a new value for the variable.
		if _, err := c.ExecContext(ctx, fmt.Sprintf("SET RETRY_ABORTS_INTERNALLY = %v", tc.set)); err != nil {
			t.Fatalf("failed to set value for retry_aborts_internally: %v", err)
		}
	}

	// Verify that the value cannot be set during an active transaction.
	tx, _ := c.BeginTx(ctx, nil)
	// Execute a statement to activate the transaction.
	if _, err := c.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	_, err = c.ExecContext(ctx, "SET RETRY_ABORTS_INTERNALLY = TRUE")
	if g, w := spanner.ErrCode(err), codes.FailedPrecondition; g != w {
		t.Fatalf("error code mismatch for setting retry_aborts_internally during a transaction\nGot: %v\nWant: %v", g, w)
	}
	_ = tx.Rollback()

	// Verify that the value can be set at the start of a transaction
	// before any statements have been executed.
	tx, _ = c.BeginTx(ctx, nil)
	if _, err = c.ExecContext(ctx, "SET RETRY_ABORTS_INTERNALLY = TRUE"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
}

func TestPartitionedDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to obtain a connection: %v", err)
	}
	defer silentClose(c)

	if _, err := c.ExecContext(ctx, "set autocommit_dml_mode = 'Partitioned_Non_Atomic'"); err != nil {
		t.Fatalf("could not set autocommit dml mode: %v", err)
	}

	_ = server.TestSpanner.PutStatementResult("DELETE FROM Foo WHERE TRUE", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 200,
	})
	// The following statement should be executed using PDML instead of DML.
	res, err := c.ExecContext(ctx, "DELETE FROM Foo WHERE TRUE")
	if err != nil {
		t.Fatalf("could not execute DML statement: %v", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 200 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 200)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	beginRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{}))
	var beginPdml *sppb.BeginTransactionRequest
	for _, req := range beginRequests {
		if req.(*sppb.BeginTransactionRequest).Options.GetPartitionedDml() != nil {
			beginPdml = req.(*sppb.BeginTransactionRequest)
			break
		}
	}
	if beginPdml == nil {
		t.Fatal("no begin request for Partitioned DML found")
	}
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil || req.Transaction.GetId() == nil {
		t.Fatal("missing transaction id for sql request")
	}
	if !server.TestSpanner.IsPartitionedDmlTransaction(req.Transaction.GetId()) {
		t.Fatalf("sql request did not use a PDML transaction")
	}
}

func TestAutocommitBatchDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to obtain a connection: %v", err)
	}
	defer silentClose(c)

	if _, err := c.ExecContext(ctx, "START BATCH DML"); err != nil {
		t.Fatalf("could not start a DML batch: %v", err)
	}
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (1, 'One')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (2, 'Two')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})

	// The following statements should be batched locally and only sent to Spanner once
	// 'RUN BATCH' is executed.
	res, err := c.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (1, 'One')")
	if err != nil {
		t.Fatalf("could not execute DML statement: %v", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 0 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 0)
	}
	res, err = c.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (2, 'Two')")
	if err != nil {
		t.Fatalf("could not execute DML statement: %v", err)
	}
	affected, _ = res.RowsAffected()
	if affected != 0 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 0)
	}

	// There should be no ExecuteSqlRequest statements on the server.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 0 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 0)
	}
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if len(batchRequests) != 0 {
		t.Fatalf("BatchDML requests count mismatch\nGot: %v\nWant: %v", len(batchRequests), 0)
	}

	// Execute a RUN BATCH statement. This should trigger a BatchDML request followed by a Commit request.
	res, err = c.ExecContext(ctx, "RUN BATCH")
	if err != nil {
		t.Fatalf("failed to execute RUN BATCH: %v", err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		t.Fatalf("could not get rows affected from batch: %v", err)
	}
	if affected != 2 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 2)
	}

	requests = drainRequestsFromServer(server.TestSpanner)
	// There should still be no ExecuteSqlRequests on the server.
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 0 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 0)
	}
	batchRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if len(batchRequests) != 1 {
		t.Fatalf("BatchDML requests count mismatch\nGot: %v\nWant: %v", len(batchRequests), 1)
	}
	if !batchRequests[0].(*sppb.ExecuteBatchDmlRequest).LastStatements {
		t.Fatal("last statements flag not set")
	}
	// The transaction should also have been committed.
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if len(commitRequests) != 1 {
		t.Fatalf("Commit requests count mismatch\nGot: %v\nWant: %v", len(commitRequests), 1)
	}
}

func TestExecuteBatchDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (1, 'One')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (2, 'Two')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})

	res, err := ExecuteBatchDml(ctx, db, func(ctx context.Context, batch DmlBatch) error {
		if err := batch.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (1, 'One')"); err != nil {
			return err
		}
		if err := batch.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (2, 'Two')"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to execute dml batch: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("could not get rows affected from batch: %v", err)
	}
	if g, w := affected, int64(2); g != w {
		t.Fatalf("affected rows mismatch\n Got: %v\nWant: %v", g, w)
	}
	batchAffected, err := res.BatchRowsAffected()
	if err != nil {
		t.Fatalf("could not get batch rows affected from batch: %v", err)
	}
	if g, w := batchAffected, []int64{1, 1}; !cmp.Equal(g, w) {
		t.Fatalf("affected batch rows mismatch\n Got: %v\nWant: %v", g, w)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	// There should be no ExecuteSqlRequests on the server.
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 0; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(batchRequests), 1; g != w {
		t.Fatalf("BatchDML requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if !batchRequests[0].(*sppb.ExecuteBatchDmlRequest).LastStatements {
		t.Fatal("last statements flag not set")
	}
	// The transaction should have been committed.
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("Commit requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestExecuteBatchDmlError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (1, 'One')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	c, err := db.Conn(ctx)
	defer func() { _ = c.Close() }()
	if err != nil {
		t.Fatalf("failed to obtain connection: %v", err)
	}

	_, err = ExecuteBatchDmlOnConn(ctx, c, func(ctx context.Context, batch DmlBatch) error {
		if err := batch.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (1, 'One')"); err != nil {
			return err
		}
		return fmt.Errorf("test error")
	})
	if err == nil {
		t.Fatalf("failed to execute dml batch: %v", err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	// There should be no requests on the server.
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(batchRequests), 0; g != w {
		t.Fatalf("BatchDML requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 0; g != w {
		t.Fatalf("Commit requests count mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the connection is not in a batch, and that it can be used for other statements.
	if err := c.Raw(func(driverConn any) error {
		spannerConn, ok := driverConn.(SpannerConn)
		if !ok {
			return fmt.Errorf("driver connection is not a SpannerConn")
		}
		if spannerConn.InDMLBatch() {
			return fmt.Errorf("connection is still in a batch")
		}
		return nil
	}); err != nil {
		t.Fatalf("check if connection is in a batch failed: %v", err)
	}
	res, err := c.ExecContext(ctx, `INSERT INTO Foo (Id, Val) VALUES (1, 'One')`)
	if err != nil {
		t.Fatalf("failed to execute dml statement: %v", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		t.Fatalf("failed to obtain rows affected: %v", err)
	} else {
		if g, w := affected, int64(1); g != w {
			t.Fatalf("affected rows mismatch\n Got: %v\nWant: %v", g, w)
		}
	}
}

func TestTransactionBatchDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to start transaction: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "START BATCH DML"); err != nil {
		t.Fatalf("could not start a DML batch: %v", err)
	}
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (1, 'One')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (2, 'Two')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})

	// The following statements should be batched locally and only sent to Spanner once
	// 'RUN BATCH' is executed.
	res, err := tx.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (1, 'One')")
	if err != nil {
		t.Fatalf("could not execute DML statement: %v", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 0 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 0)
	}
	res, err = tx.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (2, 'Two')")
	if err != nil {
		t.Fatalf("could not execute DML statement: %v", err)
	}
	affected, _ = res.RowsAffected()
	if affected != 0 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 0)
	}

	// There should be no ExecuteSqlRequest statements on the server.
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 0 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 0)
	}
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if len(batchRequests) != 0 {
		t.Fatalf("BatchDML requests count mismatch\nGot: %v\nWant: %v", len(batchRequests), 0)
	}

	// Execute a RUN BATCH statement. This should trigger a BatchDML request.
	res, err = tx.ExecContext(ctx, "RUN BATCH")
	if err != nil {
		t.Fatalf("failed to execute RUN BATCH: %v", err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		t.Fatalf("could not get rows affected from batch: %v", err)
	}
	if affected != 2 {
		t.Fatalf("affected rows mismatch\nGot: %v\nWant: %v", affected, 2)
	}

	requests = drainRequestsFromServer(server.TestSpanner)
	// There should still be no ExecuteSqlRequests on the server.
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 0 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 0)
	}
	batchRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if len(batchRequests) != 1 {
		t.Fatalf("BatchDML requests count mismatch\nGot: %v\nWant: %v", len(batchRequests), 1)
	}
	if batchRequests[0].(*sppb.ExecuteBatchDmlRequest).LastStatements {
		t.Fatal("last statements flag set")
	}
	// The transaction should still be active.
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if len(commitRequests) != 0 {
		t.Fatalf("Commit requests count mismatch\nGot: %v\nWant: %v", len(commitRequests), 0)
	}

	// Executing another DML statement on the same transaction now that the batch has been
	// executed should cause the statement to be sent to Spanner.
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (3, 'Three')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	if _, err := tx.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (3, 'Three')"); err != nil {
		t.Fatalf("failed to execute DML statement after batch: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit transaction after batch: %v", err)
	}

	requests = drainRequestsFromServer(server.TestSpanner)
	// There should now be one ExecuteSqlRequests on the server.
	sqlRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if len(sqlRequests) != 1 {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", len(sqlRequests), 1)
	}
	// There should be no new Batch DML requests.
	batchRequests = requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if len(batchRequests) != 0 {
		t.Fatalf("BatchDML requests count mismatch\nGot: %v\nWant: %v", len(batchRequests), 0)
	}
	// The transaction should now be committed.
	commitRequests = requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if len(commitRequests) != 1 {
		t.Fatalf("Commit requests count mismatch\nGot: %v\nWant: %v", len(commitRequests), 1)
	}
}

func TestExecuteBatchDmlTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (1, 'One')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})
	_ = server.TestSpanner.PutStatementResult("INSERT INTO Foo (Id, Val) VALUES (2, 'Two')", &testutil.StatementResult{
		Type:        testutil.StatementResultUpdateCount,
		UpdateCount: 1,
	})

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to obtain connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	res, err := ExecuteBatchDmlOnConn(ctx, conn, func(ctx context.Context, batch DmlBatch) error {
		if err := batch.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (1, 'One')"); err != nil {
			return err
		}
		if err := batch.ExecContext(ctx, "INSERT INTO Foo (Id, Val) VALUES (2, 'Two')"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to execute dml batch: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("could not get rows affected from batch: %v", err)
	}
	if g, w := affected, int64(2); g != w {
		t.Fatalf("affected rows mismatch\n Got: %v\nWant: %v", g, w)
	}
	batchAffected, err := res.BatchRowsAffected()
	if err != nil {
		t.Fatalf("could not get batch rows affected from batch: %v", err)
	}
	if g, w := batchAffected, []int64{1, 1}; !cmp.Equal(g, w) {
		t.Fatalf("affected batch rows mismatch\n Got: %v\nWant: %v", g, w)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit transaction after batch: %v", err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	// There should be no ExecuteSqlRequests on the server.
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 0; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(batchRequests), 1; g != w {
		t.Fatalf("BatchDML requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if batchRequests[0].(*sppb.ExecuteBatchDmlRequest).LastStatements {
		t.Fatal("last statements flag was set, this should not happen for batches in a transaction")
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("Commit requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestCommitTimestamp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to start transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	// Get the commit timestamp from the connection.
	// We do this in a simple loop to verify that we can get it multiple times.
	for i := 0; i < 2; i++ {
		var ts time.Time
		if err := conn.Raw(func(driverConn interface{}) error {
			ts, err = driverConn.(SpannerConn).CommitTimestamp()
			return err
		}); err != nil {
			t.Fatalf("failed to get commit timestamp: %v", err)
		}
		if cmp.Equal(time.Time{}, ts) {
			t.Fatalf("got zero commit timestamp: %v", ts)
		}
	}
}

func TestCommitTimestampAutocommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatalf("failed to execute update: %v", err)
	}
	// Get the commit timestamp from the connection.
	// We do this in a simple loop to verify that we can get it multiple times.
	for i := 0; i < 2; i++ {
		var ts time.Time
		if err := conn.Raw(func(driverConn interface{}) error {
			ts, err = driverConn.(SpannerConn).CommitTimestamp()
			return err
		}); err != nil {
			t.Fatalf("failed to get commit timestamp: %v", err)
		}
		if cmp.Equal(time.Time{}, ts) {
			t.Fatalf("got zero commit timestamp: %v", ts)
		}
	}
}

func TestCommitTimestampFailsAfterRollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to start transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	// Try to get the commit timestamp from the connection.
	err = conn.Raw(func(driverConn interface{}) error {
		_, err = driverConn.(SpannerConn).CommitTimestamp()
		return err
	})
	if g, w := spanner.ErrCode(err), codes.FailedPrecondition; g != w {
		t.Fatalf("get commit timestamp error code mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestCommitTimestampFailsAfterAutocommitQuery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	var v string
	if err := conn.QueryRowContext(ctx, testutil.SelectFooFromBar).Scan(&v); err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}
	// Try to get the commit timestamp from the connection. This should not be possible as a query in autocommit mode
	// will not return a commit timestamp.
	err = conn.Raw(func(driverConn interface{}) error {
		_, err = driverConn.(SpannerConn).CommitTimestamp()
		return err
	})
	if g, w := spanner.ErrCode(err), codes.FailedPrecondition; g != w {
		t.Fatalf("get commit timestamp error code mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestShowVariableCommitTimestamp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to start transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	// Get the commit timestamp from the connection using a custom SQL statement.
	// We do this in a simple loop to verify that we can get it multiple times.
	for i := 0; i < 2; i++ {
		var ts time.Time
		if err := conn.QueryRowContext(ctx, "SHOW VARIABLE COMMIT_TIMESTAMP").Scan(&ts); err != nil {
			t.Fatalf("failed to get commit timestamp: %v", err)
		}
		if cmp.Equal(time.Time{}, ts) {
			t.Fatalf("got zero commit timestamp: %v", ts)
		}
	}
}

func TestMinSessions(t *testing.T) {
	t.Parallel()

	minSessions := int32(10)
	ctx := context.Background()
	db, server, teardown := setupTestDBConnectionWithParams(t, fmt.Sprintf("minSessions=%v", minSessions))
	defer teardown()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	var res int64
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&res); err != nil {
		t.Fatalf("failed to execute query on connection: %v", err)
	}
	// Wait until all sessions have been created.
	waitFor(t, func() error {
		created := int32(server.TestSpanner.TotalSessionsCreated())
		if created != minSessions {
			return fmt.Errorf("num open sessions mismatch\n Got: %d\nWant: %d", created, minSessions)
		}
		return nil
	})
	_ = conn.Close()
	_ = db.Close()

	// Verify that the connector created 10 sessions on the server.
	reqs := drainRequestsFromServer(server.TestSpanner)
	createReqs := requestsOfType(reqs, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	numCreated := int32(0)
	for _, req := range createReqs {
		numCreated += req.(*sppb.BatchCreateSessionsRequest).SessionCount
	}
	if g, w := numCreated, minSessions; g != w {
		t.Errorf("session creation count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestMaxSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnectionWithParams(t, "minSessions=0;maxSessions=2")
	defer teardown()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Errorf("failed to get a connection: %v", err)
			}
			var res int64
			if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&res); err != nil {
				t.Errorf("failed to execute query on connection: %v", err)
			}
			_ = conn.Close()
		}()
	}
	wg.Wait()

	// Verify that the connector only created 2 sessions on the server.
	reqs := drainRequestsFromServer(server.TestSpanner)
	createReqs := requestsOfType(reqs, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	numCreated := int32(0)
	for _, req := range createReqs {
		numCreated += req.(*sppb.BatchCreateSessionsRequest).SessionCount
	}
	if g, w := numCreated, int32(2); g != w {
		t.Errorf("session creation count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestClientReuse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnectionWithParams(t, "minSessions=2")
	defer teardown()

	// Repeatedly get a connection and close it using the same DB instance. These
	// connections should all share the same Spanner client, and only initialized
	// one session pool.
	for i := 0; i < 5; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("failed to get a connection: %v", err)
		}
		var res int64
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&res); err != nil {
			t.Fatalf("failed to execute query on connection: %v", err)
		}
		_ = conn.Close()
	}
	// Verify that the connector only created 2 sessions on the server.
	reqs := drainRequestsFromServer(server.TestSpanner)
	createReqs := requestsOfType(reqs, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	numCreated := int32(0)
	for _, req := range createReqs {
		numCreated += req.(*sppb.BatchCreateSessionsRequest).SessionCount
	}
	if g, w := numCreated, int32(2); g != w {
		t.Errorf("session creation count mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Now close the DB instance and create a new DB connection.
	// This should cause the first Spanner client to be closed and
	// a new one to be opened.
	_ = db.Close()

	db, err := sql.Open(
		"spanner",
		fmt.Sprintf("%s/projects/p/instances/i/databases/d?useplaintext=true;minSessions=2", server.Address))
	if err != nil {
		t.Fatalf("failed to open new DB instance: %v", err)
	}
	var res int64
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&res); err != nil {
		t.Fatalf("failed to execute query on db: %v", err)
	}
	reqs = drainRequestsFromServer(server.TestSpanner)
	createReqs = requestsOfType(reqs, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	numCreated = int32(0)
	for _, req := range createReqs {
		numCreated += req.(*sppb.BatchCreateSessionsRequest).SessionCount
	}
	if g, w := numCreated, int32(2); g != w {
		t.Errorf("session creation count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestStressClientReuse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, server, teardown := setupTestDBConnection(t)
	defer teardown()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	numSessions := 10
	numClients := 5
	numParallel := 50
	var wg sync.WaitGroup
	for clientIndex := 0; clientIndex < numClients; clientIndex++ {
		// Open a DB using a dsn that contains a meaningless number. This will ensure that
		// the underlying client will be different from the other connections that use a
		// different number.
		db, err := sql.Open("spanner",
			fmt.Sprintf("%s/projects/p/instances/i/databases/d?useplaintext=true;minSessions=%v;maxSessions=%v;randomNumber=%v", server.Address, numSessions, numSessions, clientIndex))
		if err != nil {
			t.Fatalf("failed to open DB: %v", err)
		}
		// Execute random operations in parallel on the database.
		for i := 0; i < numParallel; i++ {
			doUpdate := rng.Int()%2 == 0

			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := db.Conn(ctx)
				if err != nil {
					t.Errorf("failed to get a connection: %v", err)
				}
				if doUpdate {
					if _, err := conn.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
						t.Errorf("failed to execute update on connection: %v", err)
					}
				} else {
					var res int64
					if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&res); err != nil {
						t.Errorf("failed to execute query on connection: %v", err)
					}
				}
				_ = conn.Close()
			}()
		}
	}
	wg.Wait()

	// Verify that each unique connection string created numSessions (10) sessions on the server.
	reqs := drainRequestsFromServer(server.TestSpanner)
	createReqs := requestsOfType(reqs, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	numCreated := int32(0)
	for _, req := range createReqs {
		numCreated += req.(*sppb.BatchCreateSessionsRequest).SessionCount
	}
	if g, w := numCreated, int32(numSessions*numClients); g != w {
		t.Errorf("session creation count mismatch\n Got: %v\nWant: %v", g, w)
	}
	sqlReqs := requestsOfType(reqs, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlReqs), numClients*numParallel; g != w {
		t.Errorf("ExecuteSql request count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestExcludeTxnFromChangeStreams_AutoCommitUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	var exclude bool
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE EXCLUDE_TXN_FROM_CHANGE_STREAMS").Scan(&exclude); err != nil {
		t.Fatalf("failed to get exclude setting: %v", err)
	}
	if g, w := exclude, false; g != w {
		t.Fatalf("exclude_txn_from_change_streams mismatch\n Got: %v\nWant: %v", g, w)
	}
	if _, err := conn.ExecContext(ctx, "set exclude_txn_from_change_streams = true"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if _, ok := req.Transaction.Selector.(*sppb.TransactionSelector_Begin); !ok {
		t.Fatalf("unsupported transaction type %T", req.Transaction.Selector)
	}
	begin := req.Transaction.Selector.(*sppb.TransactionSelector_Begin)
	if !begin.Begin.ExcludeTxnFromChangeStreams {
		t.Fatalf("missing ExcludeTxnFromChangeStreams option on BeginTransaction option")
	}
}

func TestExcludeTxnFromChangeStreams_AutoCommitBatchDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "set exclude_txn_from_change_streams = true"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "start batch dml"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "run batch"); err != nil {
		t.Fatal(err)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(batchRequests), 1; g != w {
		t.Fatalf("ExecuteBatchDmlRequest count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := batchRequests[0].(*sppb.ExecuteBatchDmlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteBatchDmlRequest")
	}
	if _, ok := req.Transaction.Selector.(*sppb.TransactionSelector_Begin); !ok {
		t.Fatalf("unsupported transaction type %T", req.Transaction.Selector)
	}
	begin := req.Transaction.Selector.(*sppb.TransactionSelector_Begin)
	if !begin.Begin.ExcludeTxnFromChangeStreams {
		t.Fatalf("missing ExcludeTxnFromChangeStreams option on BeginTransaction option")
	}
}

func TestExcludeTxnFromChangeStreams_PartitionedDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	if _, err := conn.ExecContext(ctx, "set exclude_txn_from_change_streams = true"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "set autocommit_dml_mode = 'partitioned_non_atomic'"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	beginRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{}))
	if g, w := len(beginRequests), 1; g != w {
		t.Fatalf("BeginTransactionRequest count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := beginRequests[0].(*sppb.BeginTransactionRequest)
	if !req.Options.ExcludeTxnFromChangeStreams {
		t.Fatalf("missing ExcludeTxnFromChangeStreams option on BeginTransaction option")
	}
}

func TestExcludeTxnFromChangeStreams_Transaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	var exclude bool
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE EXCLUDE_TXN_FROM_CHANGE_STREAMS").Scan(&exclude); err != nil {
		t.Fatalf("failed to get exclude setting: %v", err)
	}
	if g, w := exclude, false; g != w {
		t.Fatalf("exclude_txn_from_change_streams mismatch\n Got: %v\nWant: %v", g, w)
	}
	_, _ = conn.ExecContext(ctx, "set exclude_txn_from_change_streams = true")
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.ExecContext(ctx, testutil.UpdateBarSetFoo)
	_ = tx.Commit()

	requests := drainRequestsFromServer(server.TestSpanner)
	beginRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BeginTransactionRequest{}))
	if g, w := len(beginRequests), 0; g != w {
		t.Fatalf("BeginTransactionRequest count mismatch\n Got: %v\nWant: %v", g, w)
	}
	executeRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(executeRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequest count mismatch\n Got: %v\nWant: %v", g, w)
	}
	req := executeRequests[0].(*sppb.ExecuteSqlRequest)
	if req.GetTransaction() == nil || req.GetTransaction().GetBegin() == nil {
		t.Fatal("missing BeginTransaction option on ExecuteSqlRequest")
	}
	if !req.GetTransaction().GetBegin().ExcludeTxnFromChangeStreams {
		t.Fatalf("missing ExcludeTxnFromChangeStreams option on BeginTransaction option")
	}

	// Verify that the flag is NOT reset after the transaction.
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE EXCLUDE_TXN_FROM_CHANGE_STREAMS").Scan(&exclude); err != nil {
		t.Fatalf("failed to get exclude setting: %v", err)
	}
	if g, w := exclude, true; g != w {
		t.Fatalf("exclude_txn_from_change_streams mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestTag_Query_AutoCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	_, _ = conn.ExecContext(ctx, "set statement_tag = 'tag_1'")
	iter, err := conn.QueryContext(ctx, testutil.SelectFooFromBar)
	if err != nil {
		t.Fatalf("failed to execute query: %v", err)
	}
	// Just consume the results to ensure that the query is executed.
	for iter.Next() {
		if iter.Err() != nil {
			t.Fatal(iter.Err())
		}
	}
	_ = iter.Close()

	requests := drainRequestsFromServer(server.TestSpanner)
	// The ExecuteSqlRequest and CommitRequest should have a transaction tag.
	execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(execRequests), 1; g != w {
		t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
	}
	execRequest := execRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := execRequest.RequestOptions.TransactionTag, ""; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := execRequest.RequestOptions.RequestTag, "tag_1"; g != w {
		t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the tag is reset after the statement.
	var statementTag string
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE STATEMENT_TAG").Scan(&statementTag); err != nil {
		t.Fatalf("failed to get statement_tag: %v", err)
	}
	if g, w := statementTag, ""; g != w {
		t.Fatalf("statement_tag mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestTag_Update_AutoCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	_, _ = conn.ExecContext(ctx, "set transaction_tag = 'my_transaction_tag'")
	_, _ = conn.ExecContext(ctx, "set statement_tag = 'tag_1'")
	_, _ = conn.ExecContext(ctx, testutil.UpdateBarSetFoo)

	requests := drainRequestsFromServer(server.TestSpanner)
	// The ExecuteSqlRequest and CommitRequest should have a transaction tag.
	execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(execRequests), 1; g != w {
		t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
	}
	execRequest := execRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := execRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := execRequest.RequestOptions.RequestTag, "tag_1"; g != w {
		t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := commitRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the tag is reset after the statement.
	var transactionTag string
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
		t.Fatalf("failed to get transaction_tag: %v", err)
	}
	if g, w := transactionTag, ""; g != w {
		t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestTag_AutoCommit_BatchDml(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	_, _ = conn.ExecContext(ctx, "set transaction_tag = 'my_transaction_tag'")
	_, _ = conn.ExecContext(ctx, "set statement_tag = 'tag_1'")
	_, _ = conn.ExecContext(ctx, "start batch dml")
	_, _ = conn.ExecContext(ctx, testutil.UpdateBarSetFoo)
	_, _ = conn.ExecContext(ctx, testutil.UpdateBarSetFoo)
	_, _ = conn.ExecContext(ctx, "run batch")

	requests := drainRequestsFromServer(server.TestSpanner)
	// The ExecuteBatchDmlRequest and CommitRequest should have a transaction tag.
	execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(execRequests), 1; g != w {
		t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
	}
	execRequest := execRequests[0].(*sppb.ExecuteBatchDmlRequest)
	if g, w := execRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := execRequest.RequestOptions.RequestTag, "tag_1"; g != w {
		t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := commitRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the tag is reset after the statement.
	var transactionTag string
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
		t.Fatalf("failed to get transaction_tag: %v", err)
	}
	if g, w := transactionTag, ""; g != w {
		t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestTag_ReadWriteTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	var transactionTag string
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
		t.Fatalf("failed to get transaction tag: %v", err)
	}
	if g, w := transactionTag, ""; g != w {
		t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	_, _ = conn.ExecContext(ctx, "set transaction_tag = 'my_transaction_tag'")
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.ExecContext(ctx, "set statement_tag = 'tag_1'")
	rows, _ := tx.QueryContext(ctx, testutil.SelectFooFromBar)
	for rows.Next() {
	}
	_ = rows.Close()

	_, _ = tx.ExecContext(ctx, "set statement_tag = 'tag_2'")
	_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)

	_, _ = tx.ExecContext(ctx, "set statement_tag = 'tag_3'")
	_, _ = tx.ExecContext(ctx, "start batch dml")
	_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
	_, _ = tx.ExecContext(ctx, "run batch")
	_ = tx.Commit()

	requests := drainRequestsFromServer(server.TestSpanner)
	// The ExecuteSqlRequest and CommitRequest should have a transaction tag.
	execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(execRequests), 2; g != w {
		t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
	}
	for i := 0; i < len(execRequests); i++ {
		execRequest := execRequests[i].(*sppb.ExecuteSqlRequest)
		if g, w := execRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
			t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
		}
		if g, w := execRequest.RequestOptions.RequestTag, fmt.Sprintf("tag_%d", (i%2)+1); g != w {
			t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
		}
	}

	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
	if g, w := len(batchRequests), 1; g != w {
		t.Fatalf("number of batch request mismatch\n Got: %v\nWant: %v", g, w)
	}
	batchRequest := batchRequests[0].(*sppb.ExecuteBatchDmlRequest)
	if g, w := batchRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := batchRequest.RequestOptions.RequestTag, "tag_3"; g != w {
		t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
	}

	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := commitRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
		t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the tag is reset after the transaction.
	if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
		t.Fatalf("failed to get transaction_tag: %v", err)
	}
	if g, w := transactionTag, ""; g != w {
		t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestTag_ReadWriteTransaction_Retry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	for _, useArgs := range []bool{false, true} {
		var transactionTag string
		if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
			t.Fatalf("failed to get transaction tag: %v", err)
		}
		if g, w := transactionTag, ""; g != w {
			t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
		}
		_, _ = conn.ExecContext(ctx, "set transaction_tag = 'my_transaction_tag'")
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}

		var rows *sql.Rows
		if useArgs {
			rows, _ = tx.QueryContext(ctx, testutil.SelectFooFromBar, ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_1"}})
		} else {
			_, _ = tx.ExecContext(ctx, "set statement_tag='tag_1'")
			rows, _ = tx.QueryContext(ctx, testutil.SelectFooFromBar)
		}
		for rows.Next() {
		}
		_ = rows.Close()

		if useArgs {
			_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo, ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_2"}})
		} else {
			_, _ = tx.ExecContext(ctx, "set statement_tag='tag_2'")
			_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
		}

		if useArgs {
			_, _ = tx.ExecContext(ctx, "start batch dml", ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_3"}})
		} else {
			_, _ = tx.ExecContext(ctx, "set statement_tag = 'tag_3'")
			_, _ = tx.ExecContext(ctx, "start batch dml")
		}
		_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
		_, _ = tx.ExecContext(ctx, "run batch")

		server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
			Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
		})
		_ = tx.Commit()

		requests := drainRequestsFromServer(server.TestSpanner)
		// The ExecuteSqlRequest and CommitRequest should have a transaction tag.
		execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
		if g, w := len(execRequests), 4; g != w {
			t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(execRequests); i++ {
			execRequest := execRequests[i].(*sppb.ExecuteSqlRequest)
			if g, w := execRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
			if g, w := execRequest.RequestOptions.RequestTag, fmt.Sprintf("tag_%d", (i%2)+1); g != w {
				t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
			}
		}

		batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
		if g, w := len(batchRequests), 2; g != w {
			t.Fatalf("number of batch request mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(batchRequests); i++ {
			batchRequest := batchRequests[i].(*sppb.ExecuteBatchDmlRequest)
			if g, w := batchRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
			if g, w := batchRequest.RequestOptions.RequestTag, "tag_3"; g != w {
				t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
			}
		}

		commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
		if g, w := len(commitRequests), 2; g != w {
			t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(commitRequests); i++ {
			commitRequest := commitRequests[i].(*sppb.CommitRequest)
			if g, w := commitRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
		}

		// Verify that the tag is reset after the transaction.
		if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
			t.Fatalf("failed to get transaction_tag: %v", err)
		}
		if g, w := transactionTag, ""; g != w {
			t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
		}
	}
}

func TestTag_RunTransaction_Retry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	conn, err := db.Conn(ctx)
	defer func() {
		if conn == nil {
			return
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}

	for _, useArgs := range []bool{false, true} {
		attempts := 0
		err = RunTransactionWithOptions(ctx, db, &sql.TxOptions{}, func(ctx context.Context, tx *sql.Tx) error {
			attempts++
			var rows *sql.Rows
			if useArgs {
				rows, _ = tx.QueryContext(ctx, testutil.SelectFooFromBar, ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_1"}})
			} else {
				_, _ = tx.ExecContext(ctx, "set statement_tag='tag_1'")
				rows, _ = tx.QueryContext(ctx, testutil.SelectFooFromBar)
			}
			for rows.Next() {
			}
			_ = rows.Close()

			if useArgs {
				_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo, ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_2"}})
			} else {
				_, _ = tx.ExecContext(ctx, "set statement_tag='tag_2'")
				_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
			}

			if useArgs {
				_, _ = tx.ExecContext(ctx, "start batch dml", ExecOptions{QueryOptions: spanner.QueryOptions{RequestTag: "tag_3"}})
			} else {
				_, _ = tx.ExecContext(ctx, "set statement_tag = 'tag_3'")
				_, _ = tx.ExecContext(ctx, "start batch dml")
			}
			_, _ = tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
			_, _ = tx.ExecContext(ctx, "run batch")
			if attempts == 1 {
				server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
					Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
				})
			}
			return nil
		}, spanner.TransactionOptions{TransactionTag: "my_transaction_tag"})
		if err != nil {
			t.Fatalf("failed to run transaction: %v", err)
		}

		requests := drainRequestsFromServer(server.TestSpanner)
		// The ExecuteSqlRequest and CommitRequest should have a transaction tag.
		execRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
		if g, w := len(execRequests), 4; g != w {
			t.Fatalf("number of execute requests mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(execRequests); i++ {
			execRequest := execRequests[i].(*sppb.ExecuteSqlRequest)
			if g, w := execRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
			if g, w := execRequest.RequestOptions.RequestTag, fmt.Sprintf("tag_%d", (i%2)+1); g != w {
				t.Fatalf("useArgs: %v, statement tag mismatch\n Got: %v\nWant: %v", useArgs, g, w)
			}
		}

		batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteBatchDmlRequest{}))
		if g, w := len(batchRequests), 2; g != w {
			t.Fatalf("number of batch request mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(batchRequests); i++ {
			batchRequest := batchRequests[i].(*sppb.ExecuteBatchDmlRequest)
			if g, w := batchRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
			if g, w := batchRequest.RequestOptions.RequestTag, "tag_3"; g != w {
				t.Fatalf("statement tag mismatch\n Got: %v\nWant: %v", g, w)
			}
		}

		commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
		if g, w := len(commitRequests), 2; g != w {
			t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
		}
		for i := 0; i < len(commitRequests); i++ {
			commitRequest := commitRequests[i].(*sppb.CommitRequest)
			if g, w := commitRequest.RequestOptions.TransactionTag, "my_transaction_tag"; g != w {
				t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
			}
		}

		// Verify that the transaction tag is reset after the transaction.
		var transactionTag string
		if err := conn.QueryRowContext(ctx, "SHOW VARIABLE TRANSACTION_TAG").Scan(&transactionTag); err != nil {
			t.Fatalf("failed to get transaction_tag: %v", err)
		}
		if g, w := transactionTag, ""; g != w {
			t.Fatalf("transaction_tag mismatch\n Got: %v\nWant: %v", g, w)
		}
	}
}

func TestTag_RunTransactionWithOptions_IsNotSticky(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	if err := RunTransactionWithOptions(ctx, db, &sql.TxOptions{}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, testutil.UpdateBarSetFoo)
		if err != nil {
			return err
		}
		return nil
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{ReturnCommitStats: true},
	}); err != nil {
		t.Fatal(err)
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequest := commitRequests[0].(*sppb.CommitRequest)
	if g, w := commitRequest.ReturnCommitStats, true; g != w {
		t.Fatalf("return_commit_stats mismatch\n Got: %v\nWant: %v", g, w)
	}

	// Verify that the transaction options are not used for the next transaction.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, testutil.UpdateBarSetFoo); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	requests = drainRequestsFromServer(server.TestSpanner)
	commitRequests = requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("number of commit request mismatch\n Got: %v\nWant: %v", g, w)
	}
	commitRequest = commitRequests[0].(*sppb.CommitRequest)
	// ReturnCommitStats should be false for this transaction.
	if g, w := commitRequest.ReturnCommitStats, false; g != w {
		t.Fatalf("return_commit_stats mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestMaxIdleConnectionsNonZero(t *testing.T) {
	t.Parallel()

	// Set MinSessions=1, so we can use the number of BatchCreateSessions requests as an indication
	// of the number of clients that was created.
	db, server, teardown := setupTestDBConnectionWithParams(t, "MinSessions=1")
	defer teardown()

	db.SetMaxIdleConns(2)
	for i := 0; i < 2; i++ {
		openAndCloseConn(t, db)
	}

	// Verify that only one client was created.
	// This happens because we have a non-zero value for the number of idle connections.
	requests := drainRequestsFromServer(server.TestSpanner)
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	if g, w := len(batchRequests), 1; g != w {
		t.Fatalf("BatchCreateSessions requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func TestMaxIdleConnectionsZero(t *testing.T) {
	t.Parallel()

	// Set MinSessions=1, so we can use the number of BatchCreateSessions requests as an indication
	// of the number of clients that was created.
	db, server, teardown := setupTestDBConnectionWithParams(t, "MinSessions=1")
	defer teardown()

	db.SetMaxIdleConns(0)
	for i := 0; i < 2; i++ {
		openAndCloseConn(t, db)
	}

	// Verify that two clients were created and closed.
	// This should happen because we do not keep any idle connections open.
	requests := drainRequestsFromServer(server.TestSpanner)
	batchRequests := requestsOfType(requests, reflect.TypeOf(&sppb.BatchCreateSessionsRequest{}))
	if g, w := len(batchRequests), 2; g != w {
		t.Fatalf("BatchCreateSessions requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
}

func openAndCloseConn(t *testing.T, db *sql.DB) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	defer func() {
		err = conn.Close()
		if err != nil {
			t.Fatalf("failed to close connection: %v", err)
		}
	}()

	var result int64
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		t.Fatalf("failed to select: %v", err)
	}
	if result != 1 {
		t.Fatalf("expected 1 got %v", result)
	}
}

func TestCannotReuseClosedConnector(t *testing.T) {
	// Note: This test cannot be parallel, as it inspects the size of the shared
	// map of connectors in the driver. There is no guarantee how many connectors
	// will be open when the test is running, if there are also other tests running
	// in parallel.

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("failed to get a connection: %v", err)
	}
	_ = conn.Close()
	connectors := db.Driver().(*Driver).connectors
	if g, w := len(connectors), 1; g != w {
		t.Fatal("underlying connector has not been created")
	}
	var connector *connector
	for _, v := range connectors {
		connector = v
	}
	if connector == nil {
		t.Fatal("no connector found")
	}
	if connector.closed {
		t.Fatal("connector is closed")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("failed to close connector: %v", err)
	}
	_, err = db.Conn(ctx)
	if err == nil {
		t.Fatal("missing error for getting a connection from a closed connector")
	}
	if g, w := err.Error(), "sql: database is closed"; g != w {
		t.Fatalf("error mismatch for getting a connection from a closed connector\n Got: %v\nWant: %v", g, w)
	}
	// Verify that the underlying connector also has been closed.
	if g, w := len(connectors), 0; g != w {
		t.Fatal("underlying connector has not been closed")
	}
	if !connector.closed {
		t.Fatal("connector is not closed")
	}
}

func TestRunTransaction(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			return err
		}
		defer silentClose(rows)
		// Verify that internal retries are disabled during RunTransaction
		txi := reflect.ValueOf(tx).Elem().FieldByName("txi")
		rwTx := (*readWriteTransaction)(txi.Elem().UnsafePointer())
		// Verify that getting the transaction through reflection worked.
		if g, w := rwTx.ctx, ctx; g != w {
			return fmt.Errorf("getting the transaction through reflection failed")
		}
		if rwTx.retryAborts {
			return fmt.Errorf("internal retries should be disabled during RunTransaction")
		}

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				return fmt.Errorf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Verify that internal retries are still enabled after RunTransaction
	row := db.QueryRow("show variable retry_aborts_internally")
	var retry bool
	if err := row.Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if !retry {
		t.Fatal("internal retries should be enabled after RunTransaction")
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestRunTransactionCommitAborted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	attempts := 0
	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		attempts++
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			return err
		}
		defer silentClose(rows)

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				return fmt.Errorf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Instruct the mock server to abort the transaction.
		if attempts == 1 {
			server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
				Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	// There should be two requests, as the transaction is aborted and retried.
	if g, w := len(sqlRequests), 2; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 2; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	for i := 0; i < 2; i++ {
		req := sqlRequests[i].(*sppb.ExecuteSqlRequest)
		if req.Transaction == nil {
			t.Fatalf("missing transaction for ExecuteSqlRequest")
		}
		if i == 0 {
			if req.Transaction.GetBegin() == nil {
				t.Fatalf("missing begin selector for ExecuteSqlRequest")
			}
		} else {
			// The retried transaction uses an explicit BeginTransaction RPC.
			if req.Transaction.GetId() == nil {
				t.Fatalf("missing id selector for ExecuteSqlRequest")
			}
			commitReq := commitRequests[i].(*sppb.CommitRequest)
			if c, e := commitReq.GetTransactionId(), req.Transaction.GetId(); !cmp.Equal(c, e) {
				t.Fatalf("transaction id mismatch\nCommit: %c\nExecute: %v", c, e)
			}
		}
	}
}

func TestRunTransactionQueryAborted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	attempts := 0
	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		attempts++
		// Instruct the mock server to abort the transaction.
		if attempts == 1 {
			server.TestSpanner.PutExecutionTime(testutil.MethodExecuteStreamingSql, testutil.SimulatedExecutionTime{
				Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
			})
		}
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			return err
		}
		defer silentClose(rows)

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				return fmt.Errorf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	// There should be two ExecuteSql requests, as the transaction is aborted and retried.
	if g, w := len(sqlRequests), 2; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	// There should be only 1 CommitRequest, as the transaction is aborted before
	// the first commit attempt.
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[1].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetId() == nil {
		t.Fatalf("missing id selector for ExecuteSqlRequest")
	}
	commitReq := commitRequests[0].(*sppb.CommitRequest)
	if c, e := commitReq.GetTransactionId(), req.Transaction.GetId(); !cmp.Equal(c, e) {
		t.Fatalf("transaction id mismatch\nCommit: %c\nExecute: %v", c, e)
	}
}

func TestRunTransactionQueryError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		server.TestSpanner.PutExecutionTime(testutil.MethodExecuteStreamingSql, testutil.SimulatedExecutionTime{
			Errors: []error{gstatus.Error(codes.NotFound, "Table not found")},
		})
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			return err
		}
		defer silentClose(rows)

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				return fmt.Errorf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		t.Fatal("missing transaction error")
	}
	if g, w := spanner.ErrCode(err), codes.NotFound; g != w {
		t.Fatalf("error code mismatch\n Got: %v\nWant: %v", g, w)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	// There should be no CommitRequest, as the transaction failed
	if g, w := len(commitRequests), 0; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	// There is no RollbackRequest, as the transaction was never started.
	// The ExecuteSqlRequest included a BeginTransaction option, but because that
	// request failed, the transaction was not started.
	rollbackRequests := requestsOfType(requests, reflect.TypeOf(&sppb.RollbackRequest{}))
	if g, w := len(rollbackRequests), 0; g != w {
		t.Fatalf("rollback requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestRunTransactionCommitError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			return err
		}
		defer silentClose(rows)

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				return fmt.Errorf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Add an error for the Commit RPC. This will make the transaction fail,
		// as the commit fails.
		server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
			Errors: []error{gstatus.Error(codes.FailedPrecondition, "Unique key constraint violation")},
		})
		return nil
	})
	if err == nil {
		t.Fatal("missing transaction error")
	}
	if g, w := spanner.ErrCode(err), codes.FailedPrecondition; g != w {
		t.Fatalf("error code mismatch\n Got: %v\nWant: %v", g, w)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	// There should be no CommitRequest, as the transaction failed
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	// A Rollback request should normally not be necessary, as the Commit RPC
	// already closed the transaction. However, the Spanner client also sends
	// a RollbackRequest if a Commit fails.
	// TODO: Revisit once the client library has been checked whether it is really
	//       necessary to send a Rollback after a failed Commit.
	rollbackRequests := requestsOfType(requests, reflect.TypeOf(&sppb.RollbackRequest{}))
	if g, w := len(rollbackRequests), 1; g != w {
		t.Fatalf("rollback requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestRunTransactionPanics(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _, teardown := setupTestDBConnection(t)
	defer teardown()

	err := RunTransaction(ctx, db, nil, func(ctx context.Context, tx *sql.Tx) error {
		panic(nil)
	})
	if err == nil {
		t.Fatal("missing error from transaction runner")
	}
}

func TestTransactionWithLevelDisableRetryAborts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: WithDisableRetryAborts(sql.LevelSerializable)})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)
	for want := int64(1); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"FOO"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Simulate that the transaction was aborted.
	server.TestSpanner.PutExecutionTime(testutil.MethodCommitTransaction, testutil.SimulatedExecutionTime{
		Errors: []error{gstatus.Error(codes.Aborted, "Aborted")},
	})
	// Committing the transaction should fail, as we have disabled internal retries.
	err = tx.Commit()
	if err == nil {
		t.Fatal("missing aborted error after commit")
	}
	code := spanner.ErrCode(err)
	if w, g := code, codes.Aborted; w != g {
		t.Fatalf("error code mismatch\n Got: %v\nWant: %v", g, w)
	}

	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if req.Transaction == nil {
		t.Fatalf("missing transaction for ExecuteSqlRequest")
	}
	if req.Transaction.GetBegin() == nil {
		t.Fatalf("missing begin selector for ExecuteSqlRequest")
	}
	commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
	if g, w := len(commitRequests), 1; g != w {
		t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
}

func TestBeginReadWriteTransaction(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	db.SetMaxOpenConns(1)

	for i := 0; i < 2; i++ {
		tag := "my_tx_tag"
		tx, err := BeginReadWriteTransaction(ctx, db, ReadWriteTransactionOptions{
			DisableInternalRetries: true,
			TransactionOptions: spanner.TransactionOptions{
				TransactionTag:         tag,
				CommitPriority:         sppb.RequestOptions_PRIORITY_LOW,
				BeginTransactionOption: spanner.ExplicitBeginTransaction,
			},
		})
		if err != nil {
			t.Fatalf("failed to start transaction: %v", err)
		}

		rows, err := tx.Query(testutil.SelectFooFromBar)
		if err != nil {
			t.Fatal(err)
		}
		// Verify that internal retries are disabled during this transaction.
		txi := reflect.ValueOf(tx).Elem().FieldByName("txi")
		rwTx := (*readWriteTransaction)(txi.Elem().UnsafePointer())
		// Verify that getting the transaction through reflection worked.
		if g, w := rwTx.ctx, ctx; g != w {
			t.Fatal("getting the transaction through reflection failed")
		}
		if rwTx.retryAborts {
			t.Fatal("internal retries should be disabled during this transaction")
		}

		for want := int64(1); rows.Next(); want++ {
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			if !cmp.Equal(cols, []string{"FOO"}) {
				t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
			}
			var got int64
			err = rows.Scan(&got)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		// Verify that internal retries are still enabled after the transaction finished.
		row := db.QueryRow("show variable retry_aborts_internally")
		var retry bool
		if err := row.Scan(&retry); err != nil {
			t.Fatal(err)
		}
		if !retry {
			t.Fatal("internal retries should still be enabled")
		}

		requests := drainRequestsFromServer(server.TestSpanner)
		sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
		if g, w := len(sqlRequests), 1; g != w {
			t.Fatalf("ExecuteSqlRequests count mismatch\nGot: %v\nWant: %v", g, w)
		}
		req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
		if req.Transaction == nil {
			t.Fatalf("missing transaction for ExecuteSqlRequest")
		}
		if req.Transaction.GetId() == nil {
			t.Fatalf("missing begin selector for ExecuteSqlRequest")
		}
		if g, w := req.RequestOptions.TransactionTag, tag; g != w {
			t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
		}
		commitRequests := requestsOfType(requests, reflect.TypeOf(&sppb.CommitRequest{}))
		if g, w := len(commitRequests), 1; g != w {
			t.Fatalf("commit requests count mismatch\nGot: %v\nWant: %v", g, w)
		}
		commitReq := commitRequests[0].(*sppb.CommitRequest)
		if c, e := commitReq.GetTransactionId(), req.Transaction.GetId(); !cmp.Equal(c, e) {
			t.Fatalf("transaction id mismatch\nCommit: %c\nExecute: %v", c, e)
		}
		if g, w := commitReq.RequestOptions.TransactionTag, tag; g != w {
			t.Fatalf("transaction tag mismatch\n Got: %v\nWant: %v", g, w)
		}
		if g, w := commitReq.RequestOptions.Priority, sppb.RequestOptions_PRIORITY_LOW; g != w {
			t.Fatalf("commit priority mismatch\n Got: %v\nWant: %v", g, w)
		}
	}
}

func TestCustomClientConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	mu := sync.Mutex{}
	mu.Lock()
	interceptorInvoked := false
	routeToLeaderHeaderFound := false
	mu.Unlock()
	db, server, teardown := setupTestDBConnectionWithConfigurator(t, "", func(config *spanner.ClientConfig, opts *[]option.ClientOption) {
		config.QueryOptions = spanner.QueryOptions{Options: &sppb.ExecuteSqlRequest_QueryOptions{OptimizerVersion: "1"}}
		config.DisableRouteToLeader = true

		dopt := grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			mu.Lock()
			defer mu.Unlock()
			interceptorInvoked = true
			md, ok := metadata.FromOutgoingContext(ctx)
			if !ok {
				t.Fatalf("missing metadata for method %q", method)
			}
			if md.Get("x-goog-spanner-route-to-leader") != nil {
				routeToLeaderHeaderFound = true
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		})
		*opts = append(*opts, option.WithGRPCDialOption(dopt))
	})
	defer teardown()
	rows, err := db.QueryContext(ctx, testutil.SelectFooFromBar)
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(rows)
	rows.Next()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\nGot: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := req.QueryOptions.OptimizerVersion, "1"; g != w {
		t.Errorf("query optimizer version mismatch\n Got: %v\nWant: %v", g, w)
	}

	mu.Lock()
	defer mu.Unlock()
	if !interceptorInvoked {
		t.Errorf("interceptor was not invoked")
	}

	if routeToLeaderHeaderFound {
		t.Errorf("disabling route-to-leader did not work")
	}
}

func TestPostgreSQLDialect(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnectionWithDialect(t, databasepb.DatabaseDialect_POSTGRESQL)
	defer teardown()
	_ = server.TestSpanner.PutStatementResult(
		"SELECT * FROM Test WHERE Id=$1",
		&testutil.StatementResult{
			Type:      testutil.StatementResultResultSet,
			ResultSet: testutil.CreateSelect1ResultSet(),
		},
	)

	// The positional query parameter should be replaced with a PostgreSQL-style parameter.
	stmt, err := db.Prepare("SELECT * FROM Test WHERE Id=?")
	if err != nil {
		t.Fatal(err)
	}
	defer silentClose(stmt)

	rows, err := stmt.Query(1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for want := int64(1); rows.Next(); want++ {
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\n Got: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	requests := drainRequestsFromServer(server.TestSpanner)
	sqlRequests := requestsOfType(requests, reflect.TypeOf(&sppb.ExecuteSqlRequest{}))
	if g, w := len(sqlRequests), 1; g != w {
		t.Fatalf("sql requests count mismatch\n Got: %v\nWant: %v", g, w)
	}
	req := sqlRequests[0].(*sppb.ExecuteSqlRequest)
	if g, w := len(req.ParamTypes), 1; g != w {
		t.Fatalf("param types length mismatch\n Got: %v\nWant: %v", g, w)
	}
	if pt, ok := req.ParamTypes["p1"]; ok {
		if g, w := pt.Code, sppb.TypeCode_INT64; g != w {
			t.Fatalf("param type mismatch\n Got: %v\nWant: %v", g, w)
		}
	} else {
		t.Fatalf("no param type found for $1")
	}
	if g, w := len(req.Params.Fields), 1; g != w {
		t.Fatalf("params length mismatch\n Got: %v\nWant: %v", g, w)
	}
	if val, ok := req.Params.Fields["p1"]; ok {
		if g, w := val.GetStringValue(), "1"; g != w {
			t.Fatalf("param value mismatch\n Got: %v\nWant: %v", g, w)
		}
	} else {
		t.Fatalf("no value found for param $1")
	}
}

func TestReturnResultSetMetadata(t *testing.T) {
	t.Parallel()

	db, _, teardown := setupTestDBConnection(t)
	defer teardown()
	rows, err := db.QueryContext(context.Background(), testutil.SelectFooFromBar, ExecOptions{ReturnResultSetMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	// Verify that the first result set contains the ResultSetMetadata.
	if !rows.Next() {
		t.Fatal("no rows")
	}
	var meta *sppb.ResultSetMetadata
	if err := rows.Scan(&meta); err != nil {
		t.Fatalf("failed to scan metadata: %v", err)
	}
	if g, w := len(meta.RowType.Fields), 1; g != w {
		t.Fatalf("cols count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if rows.Next() {
		t.Fatal("more rows than expected")
	}

	// Move to the next result set, which should contain the data.
	if !rows.NextResultSet() {
		t.Fatal("no more result sets found")
	}

	for want := int64(1); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"FOO"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"FOO"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	// There should be no more result sets.
	if rows.NextResultSet() {
		t.Fatal("more result sets than expected")
	}
}

func TestReturnResultSetMetadataError(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	query := "select * from non_existing_table"
	_ = server.TestSpanner.PutStatementResult(query, &testutil.StatementResult{
		Type: testutil.StatementResultError,
		Err:  gstatus.Error(codes.NotFound, "Table not found"),
	})
	rows, err := db.QueryContext(context.Background(), query, ExecOptions{ReturnResultSetMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		t.Fatal("Next should fail")
	}
	var meta *sppb.ResultSetMetadata
	if err := rows.Scan(&meta); err == nil {
		t.Fatal("missing error when scanning metadata")
	} else {
		if g, w := spanner.ErrCode(err), codes.NotFound; g != w {
			t.Fatalf("error code mismatch\n Got: %v\nWant: %v", g, w)
		}
	}

	// Moving to the next result set fails because the query failed.
	if rows.NextResultSet() {
		t.Fatal("got unexpected next result set")
	}
}

func TestReturnResultSetStats(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()
	query := "insert into singers (name) values ('test') then return id"
	resultSet := testutil.CreateSingleColumnInt64ResultSet([]int64{42598}, "id")
	_ = server.TestSpanner.PutStatementResult(query, &testutil.StatementResult{
		Type:        testutil.StatementResultResultSet,
		ResultSet:   resultSet,
		UpdateCount: 1,
	})

	rows, err := db.QueryContext(context.Background(), query, ExecOptions{ReturnResultSetStats: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	// The first result set should contain the data.
	for want := int64(42598); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"id"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"id"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\nGot: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	// The next result set should contain the stats.
	if !rows.NextResultSet() {
		t.Fatal("missing stats result set")
	}

	// Get the stats.
	if !rows.Next() {
		t.Fatal("no stats rows")
	}
	var stats *sppb.ResultSetStats
	if err := rows.Scan(&stats); err != nil {
		t.Fatalf("failed to scan stats: %v", err)
	}
	if g, w := stats.GetRowCountExact(), int64(1); g != w {
		t.Fatalf("row count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if rows.Next() {
		t.Fatal("more rows than expected")
	}

	// There should be no more result sets.
	if rows.NextResultSet() {
		t.Fatal("more result sets than expected")
	}
}

func TestReturnResultSetMetadataAndStats(t *testing.T) {
	t.Parallel()

	db, server, teardown := setupTestDBConnection(t)
	defer teardown()

	query := "insert into singers (name) values ('test') then return id"
	resultSet := testutil.CreateSingleColumnInt64ResultSet([]int64{42598}, "id")
	_ = server.TestSpanner.PutStatementResult(query, &testutil.StatementResult{
		Type:        testutil.StatementResultResultSet,
		ResultSet:   resultSet,
		UpdateCount: 1,
	})

	rows, err := db.QueryContext(context.Background(), query, ExecOptions{
		ReturnResultSetMetadata: true,
		ReturnResultSetStats:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	// Verify that the first result set contains the ResultSetMetadata.
	if !rows.Next() {
		t.Fatal("no rows")
	}
	var meta *sppb.ResultSetMetadata
	if err := rows.Scan(&meta); err != nil {
		t.Fatalf("failed to scan metadata: %v", err)
	}
	if g, w := len(meta.RowType.Fields), 1; g != w {
		t.Fatalf("cols count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if g, w := meta.RowType.Fields[0].Name, "id"; g != w {
		t.Fatalf("column name mismatch\n Got: %v\nWant: %v", g, w)
	}
	if rows.Next() {
		t.Fatal("more rows than expected")
	}

	// Move to the next result set, which should contain the data.
	if !rows.NextResultSet() {
		t.Fatal("no more result sets found")
	}

	for want := int64(42598); rows.Next(); want++ {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if !cmp.Equal(cols, []string{"id"}) {
			t.Fatalf("cols mismatch\nGot: %v\nWant: %v", cols, []string{"id"})
		}
		var got int64
		err = rows.Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("value mismatch\n Got: %v\nWant: %v", got, want)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	// The next result set should contain the stats.
	if !rows.NextResultSet() {
		t.Fatal("missing stats result set")
	}

	// Get the stats.
	if !rows.Next() {
		t.Fatal("no stats rows")
	}
	var stats *sppb.ResultSetStats
	if err := rows.Scan(&stats); err != nil {
		t.Fatalf("failed to scan stats: %v", err)
	}
	if g, w := stats.GetRowCountExact(), int64(1); g != w {
		t.Fatalf("row count mismatch\n Got: %v\nWant: %v", g, w)
	}
	if rows.Next() {
		t.Fatal("more rows than expected")
	}

	// There should be no more result sets.
	if rows.NextResultSet() {
		t.Fatal("more result sets than expected")
	}
}

func numeric(v string) big.Rat {
	res, _ := big.NewRat(1, 1).SetString(v)
	return *res
}

func nullNumeric(valid bool, v string) spanner.NullNumeric {
	if !valid {
		return spanner.NullNumeric{}
	}
	return spanner.NullNumeric{Valid: true, Numeric: numeric(v)}
}

func date(v string) civil.Date {
	res, _ := civil.ParseDate(v)
	return res
}

func nullDate(valid bool, v string) spanner.NullDate {
	if !valid {
		return spanner.NullDate{}
	}
	return spanner.NullDate{Valid: true, Date: date(v)}
}

func nullJson(valid bool, v string) spanner.NullJSON {
	if !valid {
		return spanner.NullJSON{}
	}
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(v), &m)
	return spanner.NullJSON{Valid: true, Value: m}
}

func nullUuid(valid bool, v string) spanner.NullUUID {
	if !valid {
		return spanner.NullUUID{}
	}
	return spanner.NullUUID{Valid: true, UUID: uuid.MustParse(v)}
}

func setupTestDBConnection(t *testing.T) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	return setupTestDBConnectionWithParams(t, "")
}

func setupTestDBConnectionWithDialect(t *testing.T, dialect databasepb.DatabaseDialect) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	return setupTestDBConnectionWithParamsAndDialect(t, "", dialect)
}

func setupTestDBConnectionWithParams(t *testing.T, params string) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	return setupTestDBConnectionWithParamsAndDialect(t, params, databasepb.DatabaseDialect_GOOGLE_STANDARD_SQL)
}

func setupTestDBConnectionWithParamsAndDialect(t *testing.T, params string, dialect databasepb.DatabaseDialect) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	server, _, serverTeardown := setupMockedTestServerWithDialect(t, dialect)
	db, err := sql.Open(
		"spanner",
		fmt.Sprintf("%s/projects/p/instances/i/databases/d?useplaintext=true;%s", server.Address, params))
	if err != nil {
		serverTeardown()
		t.Fatal(err)
	}
	return db, server, func() {
		_ = db.Close()
		serverTeardown()
	}
}

func setupTestDBConnectionWithConfigurator(t *testing.T, params string, configurator func(config *spanner.ClientConfig, opts *[]option.ClientOption)) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	server, _, serverTeardown := setupMockedTestServer(t)
	dsn := fmt.Sprintf("%s/projects/p/instances/i/databases/d?useplaintext=true;%s", server.Address, params)
	config, err := ExtractConnectorConfig(dsn)
	config.Configurator = configurator
	if err != nil {
		serverTeardown()
		t.Fatal(err)
	}
	c, err := CreateConnector(config)
	if err != nil {
		serverTeardown()
		t.Fatal(err)
	}
	db = sql.OpenDB(c)
	return db, server, func() {
		_ = db.Close()
		serverTeardown()
	}
}

func setupTestDBConnectionWithConnectorConfig(t *testing.T, config ConnectorConfig) (db *sql.DB, server *testutil.MockedSpannerInMemTestServer, teardown func()) {
	server, _, serverTeardown := setupMockedTestServer(t)
	config.Host = server.Address
	if config.Params == nil {
		config.Params = make(map[string]string)
	}
	config.Params["useplaintext"] = "true"
	c, err := CreateConnector(config)
	if err != nil {
		serverTeardown()
		t.Fatal(err)
	}
	db = sql.OpenDB(c)
	return db, server, func() {
		_ = db.Close()
		serverTeardown()
	}
}

func setupMockedTestServer(t *testing.T) (server *testutil.MockedSpannerInMemTestServer, client *spanner.Client, teardown func()) {
	return setupMockedTestServerWithConfig(t, spanner.ClientConfig{})
}

func setupMockedTestServerWithDialect(t *testing.T, dialect databasepb.DatabaseDialect) (server *testutil.MockedSpannerInMemTestServer, client *spanner.Client, teardown func()) {
	return setupMockedTestServerWithConfigAndClientOptionsAndDialect(t, spanner.ClientConfig{}, []option.ClientOption{}, dialect)
}

func setupMockedTestServerWithConfig(t *testing.T, config spanner.ClientConfig) (server *testutil.MockedSpannerInMemTestServer, client *spanner.Client, teardown func()) {
	return setupMockedTestServerWithConfigAndClientOptions(t, config, []option.ClientOption{})
}

func setupMockedTestServerWithConfigAndClientOptions(t *testing.T, config spanner.ClientConfig, clientOptions []option.ClientOption) (server *testutil.MockedSpannerInMemTestServer, client *spanner.Client, teardown func()) {
	return setupMockedTestServerWithConfigAndClientOptionsAndDialect(t, config, clientOptions, databasepb.DatabaseDialect_GOOGLE_STANDARD_SQL)
}

func setupMockedTestServerWithConfigAndClientOptionsAndDialect(t *testing.T, config spanner.ClientConfig, clientOptions []option.ClientOption, dialect databasepb.DatabaseDialect) (server *testutil.MockedSpannerInMemTestServer, client *spanner.Client, teardown func()) {
	server, opts, serverTeardown := testutil.NewMockedSpannerInMemTestServer(t)
	server.SetupSelectDialectResult(dialect)

	opts = append(opts, clientOptions...)
	ctx := context.Background()
	formattedDatabase := fmt.Sprintf("projects/%s/instances/%s/databases/%s", "[PROJECT]", "[INSTANCE]", "[DATABASE]")
	config.DisableNativeMetrics = true
	client, err := spanner.NewClientWithConfig(ctx, formattedDatabase, config, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return server, client, func() {
		client.Close()
		serverTeardown()
	}
}

func filterBeginReadOnlyRequests(requests []interface{}) []*sppb.BeginTransactionRequest {
	res := make([]*sppb.BeginTransactionRequest, 0)
	for _, r := range requests {
		if req, ok := r.(*sppb.BeginTransactionRequest); ok {
			if req.Options != nil && req.Options.GetReadOnly() != nil {
				res = append(res, req)
			}
		}
	}
	return res
}

func requestsOfType(requests []interface{}, t reflect.Type) []interface{} {
	res := make([]interface{}, 0)
	for _, req := range requests {
		if reflect.TypeOf(req) == t {
			res = append(res, req)
		}
	}
	return res
}

func drainRequestsFromServer(server testutil.InMemSpannerServer) []interface{} {
	var reqs []interface{}
loop:
	for {
		select {
		case req := <-server.ReceivedRequests():
			reqs = append(reqs, req)
		default:
			break loop
		}
	}
	return reqs
}

func waitFor(t *testing.T, assert func() error) {
	t.Helper()
	timeout := 5 * time.Second
	ta := time.After(timeout)

	for {
		select {
		case <-ta:
			if err := assert(); err != nil {
				t.Fatalf("after %v waiting, got %v", timeout, err)
			}
			return
		default:
		}

		if err := assert(); err != nil {
			// Fail. Let's pause and retry.
			time.Sleep(time.Millisecond)
			continue
		}

		return
	}
}

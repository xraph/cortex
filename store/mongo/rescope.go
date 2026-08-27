package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xraph/cortex"
)

// candidateCollections are every cortex collection that might carry scope
// fields, mirroring the constants store.go already defines for indexing.
// Which of them actually have scope_l0/l1/l2/canon populated is
// discovered at runtime via the scope index Migrate creates for them
// (discoverScopedCollections), not assumed -- a partially migrated
// database is exactly the state this pass is most likely to meet, since
// later phases bring more collections under scope over time.
//
// The four cortex_a2a_* collections are deliberately absent. They were
// born with their scope fields, so no document in them has ever been
// unscoped and there is nothing here to backfill.
var candidateCollections = []string{
	colAgents, colRuns, colSteps, colToolCalls, colMemories, colCheckpoints,
	colSkills, colTraits, colBehaviors, colPersonas, colSessions,
	colOrchestrationConfigs, colOrchestrationRuns,
}

// dependentCollections are the collections whose documents must inherit
// their scope from a parent run rather than being resolved directly
// through the Rescoper: cortex_steps and cortex_tool_calls carry a
// run_id and nothing else identifying. This is deliberately a property
// of the COLLECTION, not of any one document's fields -- mongo is
// schemaless, so unlike postgres/sqlite there is no information_schema
// to derive it from, but the underlying hazard is the same one their
// tableShape.isDependent() guards against: cortex_checkpoints has both
// run_id and a (possibly blank, possibly entirely absent on documents
// written before the Go model dropped the field) tenant_id, and a
// per-document value check would silently let two checkpoints of the
// same run land in different scopes depending on which one happened to
// have a populated tenant_id.
var dependentCollections = map[string]bool{
	colSteps:     true,
	colToolCalls: true,
}

// legacyRow is one document awaiting a scope. Mongo carries no schema, so
// unlike the SQL backends the legacy identifier fields are read straight
// off the raw document rather than a fixed column set: a field the
// document doesn't have decodes to the empty string. ID stays a plain
// string here (unlike the SQL backends' `any`) because every cortex
// collection's _id is always a string TypeID under this store's own
// models -- there is no BIGSERIAL-equivalent surprise on mongo.
type legacyRow struct {
	Collection string
	ID         string
	Name       string
	AppID      string
	TenantID   string
	RunID      string
	AgentID    string
	Kind       string
	Key        string
}

func (r legacyRow) key() string { return r.Collection + "|" + r.ID }

// rawScope is a resolved scope in the same shape the scope fields are
// stored in, so a row that inherits its scope from a parent can be
// written without round-tripping through a cortex.Scope value.
type rawScope struct {
	L0, L1, L2 string
	Extra      map[string]string
	Canon      string
}

func toRawScope(sc cortex.Scope) rawScope {
	l0, l1, l2, extra := scopeColumns(sc)
	return rawScope{L0: l0, L1: l1, L2: l2, Extra: extra, Canon: sc.Canonical()}
}

// rescopeLegacyRows fills in scope fields for documents created before
// v1.8.0. The ordering is the design: scan everything, decide everything,
// check for collisions, and only then write. A half-rescoped database has
// some rows reachable and some orphaned with nothing to tell them apart,
// which is strictly worse than a migration that refused to start.
func (s *Store) rescopeLegacyRows(ctx context.Context, o cortex.MigrateOptions) error {
	collections, err := s.discoverScopedCollections(ctx)
	if err != nil {
		return fmt.Errorf("discover scoped collections: %w", err)
	}

	rows, err := s.scanUnscoped(ctx, collections)
	if err != nil {
		return fmt.Errorf("scan unscoped rows: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	// A row that only carries a run_id (a step or tool call) never calls
	// the Rescoper itself -- it inherits whatever its parent run resolves
	// to. So a rescoper is only required when at least one row needs to
	// be resolved directly: an unscoped run is itself a direct row (it
	// has no run_id of its own), so this still catches the case where a
	// dependent's parent is unscoped too.
	if hasDirectRows(rows) && o.Rescoper == nil {
		return fmt.Errorf("%w: %d rows across %d collections need one",
			cortex.ErrNoRescoper, len(rows), countCollections(rows))
	}

	resolved, err := s.resolveScopes(ctx, rows, o.Rescoper)
	if err != nil {
		return err
	}

	if err := s.detectCollisions(ctx, rows, resolved); err != nil {
		return err
	}

	return s.applyScopes(ctx, rows, resolved)
}

// discoverScopedCollections finds every candidate collection that
// actually carries the scope_l0/l1/l2 index Migrate creates for
// scope-guarded collections. A collection that doesn't have it yet, or
// doesn't exist at all, is simply skipped -- this is what let earlier
// phases bring collections under scope one at a time without this pass
// needing a hardcoded list to keep in sync.
func (s *Store) discoverScopedCollections(ctx context.Context) ([]string, error) {
	var scoped []string
	for _, col := range candidateCollections {
		has, err := s.collectionHasScopeIndex(ctx, col)
		if err != nil {
			return nil, err
		}
		if has {
			scoped = append(scoped, col)
		}
	}
	return scoped, nil
}

func (s *Store) collectionHasScopeIndex(ctx context.Context, col string) (bool, error) {
	specs, err := s.mdb.Collection(col).Indexes().ListSpecifications(ctx)
	if err != nil {
		var cmdErr mongo.CommandError
		if errors.As(err, &cmdErr) && cmdErr.Code == mongoNamespaceNotFound {
			// A genuinely fresh collection: nothing has ever been
			// migrated or written to it, so there is nothing to rescope.
			return false, nil
		}
		return false, fmt.Errorf("list indexes for %s: %w", col, err)
	}
	for _, spec := range specs {
		if spec.Name == scopeIndexName {
			return true, nil
		}
	}
	return false, nil
}

// scanUnscoped finds every document with an empty (or entirely absent)
// scope_canon field across collections.
func (s *Store) scanUnscoped(ctx context.Context, collections []string) ([]legacyRow, error) {
	filter := bson.M{"$or": []bson.M{
		{"scope_canon": bson.M{"$exists": false}},
		{"scope_canon": ""},
	}}

	var rows []legacyRow
	for _, col := range collections {
		colRows, err := s.scanCollection(ctx, col, filter)
		if err != nil {
			return nil, err
		}
		rows = append(rows, colRows...)
	}
	return rows, nil
}

func (s *Store) scanCollection(ctx context.Context, col string, filter bson.M) ([]legacyRow, error) {
	cur, err := s.mdb.Collection(col).Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", col, err)
	}
	defer func() { _ = cur.Close(ctx) }()

	var rows []legacyRow
	for cur.Next(ctx) {
		var doc bson.M
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return nil, fmt.Errorf("decode row in %s: %w", col, decodeErr)
		}
		rows = append(rows, legacyRow{
			Collection: col,
			ID:         stringField(doc, "_id"),
			Name:       stringField(doc, "name"),
			AppID:      stringField(doc, "app_id"),
			TenantID:   stringField(doc, "tenant_id"),
			RunID:      stringField(doc, "run_id"),
			AgentID:    stringField(doc, "agent_id"),
			Kind:       stringField(doc, "kind"),
			Key:        stringField(doc, "key"),
		})
	}
	return rows, cur.Err()
}

func stringField(doc bson.M, key string) string {
	v, ok := doc[key]
	if !ok {
		return ""
	}
	str, ok := v.(string)
	if !ok {
		return ""
	}
	return str
}

// resolveScopes decides a target scope for every unscoped row. Rows
// outside dependentCollections carry their own legacy identifier (app_id
// and/or tenant_id, or neither field exists at all) and go straight
// through the Rescoper, once per distinct (appID, tenantID) pair. Rows in
// dependentCollections -- cortex_steps and cortex_tool_calls -- have no
// legacy identifier of their own; they inherit whatever scope their
// parent run resolves to, so a scoped ListSteps/ListToolCalls under the
// run's scope still finds them afterward.
func (s *Store) resolveScopes(ctx context.Context, rows []legacyRow, r cortex.Rescoper) (map[string]rawScope, error) {
	resolved := make(map[string]rawScope, len(rows))
	byAppTenant := make(map[[2]string]rawScope)
	runScope := make(map[string]rawScope)
	var dependents []legacyRow

	for _, row := range rows {
		if isDependentRow(row) {
			dependents = append(dependents, row)
			continue
		}

		key := [2]string{row.AppID, row.TenantID}
		rs, ok := byAppTenant[key]
		if !ok {
			if r == nil {
				// hasDirectRows in the caller should have already turned
				// this into ErrNoRescoper; guarded here too so a future
				// change to that check fails loud instead of panicking.
				return nil, fmt.Errorf("%w: row %s in %s needs one", cortex.ErrNoRescoper, row.ID, row.Collection)
			}
			sc, rErr := r.Rescope(ctx, row.AppID, row.TenantID)
			if rErr != nil {
				return nil, fmt.Errorf("rescope (app=%q tenant=%q): %w", row.AppID, row.TenantID, rErr)
			}
			if vErr := cortex.ValidateRescopedScope(sc); vErr != nil {
				return nil, fmt.Errorf("rescope (app=%q tenant=%q): %w", row.AppID, row.TenantID, vErr)
			}
			rs = toRawScope(sc)
			byAppTenant[key] = rs
		}
		resolved[row.key()] = rs
		if row.Collection == colRuns {
			runScope[row.ID] = rs
		}
	}

	if len(dependents) == 0 {
		return resolved, nil
	}
	if err := s.resolveDependents(ctx, dependents, runScope, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// resolveDependents fills in resolved for rows that inherit their scope
// from a parent run. runScope already holds every run resolved in this
// same pass; any run_id not in there belongs to a run that was already
// scoped before this pass ran, so its current scope fields are read
// straight from the database instead of asked of the Rescoper again.
func (s *Store) resolveDependents(ctx context.Context, dependents []legacyRow, runScope, resolved map[string]rawScope) error {
	missing := make(map[string]struct{})
	for _, row := range dependents {
		if _, ok := runScope[row.RunID]; !ok {
			missing[row.RunID] = struct{}{}
		}
	}

	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		fetched, err := s.fetchRunScopes(ctx, ids)
		if err != nil {
			return err
		}
		for id, rs := range fetched {
			runScope[id] = rs
		}
	}

	for _, row := range dependents {
		rs, ok := runScope[row.RunID]
		if !ok {
			return fmt.Errorf("rescope: %s row %s references run %s, which has no resolvable scope",
				row.Collection, row.ID, row.RunID)
		}
		resolved[row.key()] = rs
	}
	return nil
}

// fetchRunScopes reads the current scope fields for a set of runs
// directly, for runs that are already scoped and so weren't part of the
// pass's own scan. Mongo carries no foreign key: a step or tool call
// referencing a run that genuinely doesn't exist is possible (unlike
// postgres/sqlite, where run_id is a real FK), so a run_id with no
// matching document just never appears in the returned map, and the
// caller reports it as unresolvable.
func (s *Store) fetchRunScopes(ctx context.Context, ids []string) (map[string]rawScope, error) {
	cur, err := s.mdb.Collection(colRuns).Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, fmt.Errorf("fetch parent run scopes: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	type runScopeDoc struct {
		ID         string            `bson:"_id"`
		ScopeL0    string            `bson:"scope_l0"`
		ScopeL1    string            `bson:"scope_l1"`
		ScopeL2    string            `bson:"scope_l2"`
		ScopeExtra map[string]string `bson:"scope_extra"`
		ScopeCanon string            `bson:"scope_canon"`
	}

	out := make(map[string]rawScope, len(ids))
	for cur.Next(ctx) {
		var doc runScopeDoc
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return nil, fmt.Errorf("scan run scope: %w", decodeErr)
		}
		// A run this pass didn't resolve itself must already carry a
		// real scope, since it wasn't in the unscoped scan.
		if doc.ScopeCanon == "" {
			return nil, fmt.Errorf("rescope: run %s has no resolvable scope", doc.ID)
		}
		extra := doc.ScopeExtra
		if extra == nil {
			extra = make(map[string]string)
		}
		out[doc.ID] = rawScope{L0: doc.ScopeL0, L1: doc.ScopeL1, L2: doc.ScopeL2, Extra: extra, Canon: doc.ScopeCanon}
	}
	return out, cur.Err()
}

// detectCollisions finds rows that would violate a (scope_canon, name) or
// working-memory (scope_canon, agent_id, kind, key) uniqueness
// expectation once written, and reports them before anything is written.
//
// This checks two things, not one: rows within THIS batch colliding with
// each other, and rows in this batch colliding with rows that are
// already scoped (from an earlier pass, or written post-v1.8.0 under a
// real scope). The latter needs a database round trip -- loadExisting*
// pre-seeds `seen`/`seenWorking` with what's already there for the
// canons this batch is about to write, before the in-batch rows are
// checked against them.
func (s *Store) detectCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope) error {
	seen := make(map[[3]string]string)        // collection+canon+name -> row id (existing or in-batch)
	seenWorking := make(map[[4]string]string) // canon+agent_id+kind+key -> row id

	if err := s.loadExistingNameCollisions(ctx, rows, resolved, seen); err != nil {
		return err
	}
	if err := s.loadExistingWorkingMemoryCollisions(ctx, rows, resolved, seenWorking); err != nil {
		return err
	}

	for _, r := range rows {
		canon := resolved[r.key()].Canon

		if r.Name != "" {
			k := [3]string{r.Collection, canon, r.Name}
			if first, ok := seen[k]; ok {
				return fmt.Errorf(
					"rescope collision: %s rows %s and %s both map to scope %q with name %q; "+
						"the rescoper must keep them in distinct scopes",
					r.Collection, first, r.ID, canon, r.Name)
			}
			seen[k] = r.ID
		}

		if r.Collection == colMemories && r.Kind == "working" {
			k := [4]string{canon, r.AgentID, r.Kind, r.Key}
			if first, ok := seenWorking[k]; ok {
				return fmt.Errorf(
					"rescope collision: cortex_memories rows %s and %s both map to scope %q "+
						"with working-memory key (agent=%q key=%q); the rescoper must keep them in distinct scopes",
					first, r.ID, canon, r.AgentID, r.Key)
			}
			seenWorking[k] = r.ID
		}
	}
	return nil
}

// loadExistingNameCollisions pre-seeds seen with every already-scoped
// document (in a name-bearing collection) whose scope_canon matches one
// of the canons this batch is about to write. Without this, a legacy row
// could be rescoped onto a scope an existing document already occupies
// under the same name, with nothing to catch it before the write.
func (s *Store) loadExistingNameCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope, seen map[[3]string]string) error {
	canonsByCollection := make(map[string]map[string]struct{})
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		canon := resolved[r.key()].Canon
		if canonsByCollection[r.Collection] == nil {
			canonsByCollection[r.Collection] = make(map[string]struct{})
		}
		canonsByCollection[r.Collection][canon] = struct{}{}
	}

	for col, canonSet := range canonsByCollection {
		canons := make([]string, 0, len(canonSet))
		for c := range canonSet {
			canons = append(canons, c)
		}
		if err := s.loadExistingNamesInCollection(ctx, col, canons, seen); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadExistingNamesInCollection(ctx context.Context, col string, canons []string, seen map[[3]string]string) error {
	cur, err := s.mdb.Collection(col).Find(ctx, bson.M{"scope_canon": bson.M{"$in": canons}})
	if err != nil {
		return fmt.Errorf("check existing names in %s: %w", col, err)
	}
	defer func() { _ = cur.Close(ctx) }()

	type nameDoc struct {
		ID    string `bson:"_id"`
		Name  string `bson:"name"`
		Canon string `bson:"scope_canon"`
	}
	for cur.Next(ctx) {
		var doc nameDoc
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return fmt.Errorf("decode existing name in %s: %w", col, decodeErr)
		}
		seen[[3]string{col, doc.Canon, doc.Name}] = doc.ID
	}
	return cur.Err()
}

// loadExistingWorkingMemoryCollisions is loadExistingNameCollisions'
// counterpart for cortex_memories, which has no `name` field and so
// never participates in the name-based check. Its uniqueness surface is
// (agent_id, kind, key, scope_canon) instead -- the same composite the
// scope-aware working-memory unique index enforces
// (workingMemoryUniqueIndexName in migrations.go).
func (s *Store) loadExistingWorkingMemoryCollisions(ctx context.Context, rows []legacyRow, resolved map[string]rawScope, seen map[[4]string]string) error {
	canonSet := make(map[string]struct{})
	for _, r := range rows {
		if r.Collection == colMemories && r.Kind == "working" {
			canonSet[resolved[r.key()].Canon] = struct{}{}
		}
	}
	if len(canonSet) == 0 {
		return nil
	}
	canons := make([]string, 0, len(canonSet))
	for c := range canonSet {
		canons = append(canons, c)
	}

	cur, err := s.mdb.Collection(colMemories).Find(ctx, bson.M{
		"kind":        "working",
		"scope_canon": bson.M{"$in": canons},
	})
	if err != nil {
		return fmt.Errorf("check existing working-memory keys: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	type workingDoc struct {
		ID      string `bson:"_id"`
		AgentID string `bson:"agent_id"`
		Key     string `bson:"key"`
		Canon   string `bson:"scope_canon"`
	}
	for cur.Next(ctx) {
		var doc workingDoc
		if decodeErr := cur.Decode(&doc); decodeErr != nil {
			return fmt.Errorf("decode existing working-memory key: %w", decodeErr)
		}
		seen[[4]string{doc.Canon, doc.AgentID, "working", doc.Key}] = doc.ID
	}
	return cur.Err()
}

// applyScopes writes every resolved scope inside one multi-document
// transaction, so a mid-write failure rolls back whole rather than
// leaving some rows rescoped and others not. This relies on mongo running
// as a replica set (or sharded cluster), which every deployment this
// store targets already is: a standalone mongod cannot run transactions
// at all.
func (s *Store) applyScopes(ctx context.Context, rows []legacyRow, resolved map[string]rawScope) error {
	sess, err := s.mdb.Client().StartSession()
	if err != nil {
		return fmt.Errorf("start rescope session: %w", err)
	}
	defer sess.EndSession(ctx)

	_, err = sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		for _, r := range rows {
			rs := resolved[r.key()]
			extra := rs.Extra
			if extra == nil {
				extra = map[string]string{}
			}
			update := bson.M{"$set": bson.M{
				"scope_l0":    rs.L0,
				"scope_l1":    rs.L1,
				"scope_l2":    rs.L2,
				"scope_extra": extra,
				"scope_canon": rs.Canon,
			}}
			if _, execErr := s.mdb.Collection(r.Collection).UpdateOne(sc, bson.M{"_id": r.ID}, update); execErr != nil {
				return nil, fmt.Errorf("rescope %s row %s: %w", r.Collection, r.ID, execErr)
			}
		}
		return nil, nil //nolint:nilnil // WithTransaction's callback contract: no result value to return
	})
	if err != nil {
		return fmt.Errorf("commit rescope transaction: %w", err)
	}
	return nil
}

func countCollections(rows []legacyRow) int {
	c := make(map[string]struct{})
	for _, r := range rows {
		c[r.Collection] = struct{}{}
	}
	return len(c)
}

// isDependentRow reports whether row must inherit its scope from a parent
// run rather than being resolved directly. Unlike the SQL backends, this
// is a lookup against dependentCollections (mongo has no schema to
// derive it from), keyed purely by which collection the row is in -- see
// dependentCollections for why that has to be collection identity and
// not a per-document field check.
func isDependentRow(row legacyRow) bool {
	return dependentCollections[row.Collection]
}

// hasDirectRows reports whether any row in rows needs to be resolved
// directly through the Rescoper, as opposed to inheriting a parent's
// scope. An unscoped run is a direct row itself, so this still requires
// a rescoper whenever a dependent's own parent is unscoped.
func hasDirectRows(rows []legacyRow) bool {
	for _, r := range rows {
		if !isDependentRow(r) {
			return true
		}
	}
	return false
}

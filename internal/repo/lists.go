package repo

import (
	"strconv"

	"github.com/go-tangra/go-tangra/v4/listquery"
)

// Sortable fields of the management lists (go-tangra 032 list contract).
// Expressions are constants; request text never reaches SQL.
var (
	// ProviderList: Search matches the name (substring, case-insensitive).
	ProviderList = listquery.Spec{Default: "name", TieBreak: "id", Fields: map[string]listquery.Field{
		"name":       {Expr: "name", Text: true, NotNull: true},
		"type":       {Expr: "type", Text: true, NotNull: true},
		"status":     {Expr: "status", NotNull: true},
		"created_at": {Expr: "create_time", DefaultDir: listquery.Desc, NotNull: true},
		"updated_at": {Expr: "update_time", DefaultDir: listquery.Desc, NotNull: true},
	}}
	// TemplateList: Search matches the name.
	TemplateList = listquery.Spec{Default: "name", TieBreak: "id", Fields: map[string]listquery.Field{
		"name":       {Expr: "name", Text: true, NotNull: true},
		"status":     {Expr: "status", NotNull: true},
		"created_at": {Expr: "create_time", DefaultDir: listquery.Desc, NotNull: true},
		"updated_at": {Expr: "update_time", DefaultDir: listquery.Desc, NotNull: true},
	}}
	// ClientList: Search matches the username or e-mail.
	ClientList = listquery.Spec{Default: "username", TieBreak: "id", Fields: map[string]listquery.Field{
		"username":      {Expr: "username", Text: true, NotNull: true},
		"authority":     {Expr: "authority", NotNull: true},
		"status":        {Expr: "status", NotNull: true},
		"last_login_at": {Expr: "last_login_time", DefaultDir: listquery.Desc},
		"created_at":    {Expr: "create_time", DefaultDir: listquery.Desc, NotNull: true},
	}}
	// BlockList: Search matches the recipient or the description.
	BlockList = listquery.Spec{Default: "created_at", TieBreak: "id", Fields: map[string]listquery.Field{
		"recipient":  {Expr: "recipient", NotNull: true},
		"status":     {Expr: "status", NotNull: true},
		"created_at": {Expr: "create_time", DefaultDir: listquery.Desc, NotNull: true},
	}}
	// MessageList: filters are MessageFilter; Search is not used.
	MessageList = listquery.Spec{Default: "created_at", TieBreak: "m.id", Fields: map[string]listquery.Field{
		"created_at": {Expr: "m.create_time", DefaultDir: listquery.Desc, NotNull: true},
		"recipient":  {Expr: "m.recipient", NotNull: true},
		"status":     {Expr: "m.status_code", NotNull: true},
	}}
)

// orderBy is the ORDER BY body for pg on spec, or legacy when no sort is
// requested.
func orderBy(spec listquery.Spec, pg Page, legacy string) string {
	if pg.Sort == "" {
		return legacy
	}
	dir := listquery.Asc
	if pg.Desc {
		dir = listquery.Desc
	}
	return listquery.Request{Sort: pg.Sort, Order: dir}.OrderBy(spec)
}

// search appends a case-insensitive substring condition over cols.
func search(where string, args []any, term string, cols ...string) (string, []any) {
	if term == "" {
		return where, args
	}
	args = append(args, "%"+likeEscape(term)+"%")
	n := "$" + strconv.Itoa(len(args))
	cond := ""
	for i, c := range cols {
		if i > 0 {
			cond += " OR "
		}
		cond += c + " ILIKE " + n
	}
	return where + " AND (" + cond + ")", args
}

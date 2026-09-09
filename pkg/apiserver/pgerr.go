package apiserver

import "github.com/jackc/pgx/v5"

// pgxNoRows is aliased so mapErr can compare against it without every caller
// importing pgx.
var pgxNoRows = pgx.ErrNoRows

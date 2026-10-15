-- Traffic at the sandboxes' URLs (docs/API.md, "Stats"): what loam-api's port
-- proxy counted, one row per sandbox per day (UTC), added to as it flushes.
CREATE TABLE sandbox_traffic (
    sandbox      text NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    day          date NOT NULL,
    requests     bigint NOT NULL DEFAULT 0,
    status_2xx   bigint NOT NULL DEFAULT 0,
    status_3xx   bigint NOT NULL DEFAULT 0,
    status_4xx   bigint NOT NULL DEFAULT 0,
    status_5xx   bigint NOT NULL DEFAULT 0,
    websockets   bigint NOT NULL DEFAULT 0,
    bytes_sent   bigint NOT NULL DEFAULT 0,
    key_requests bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (sandbox, day)
);

-- A day's distinct visitors: a hash of the day's salt, the sandbox, the client
-- address and its User-Agent. Nothing here names a client: the salt is
-- deleted when its day is over, after which a hash can't be recomputed from
-- an address.
CREATE TABLE sandbox_visitors (
    sandbox text NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    day     date NOT NULL,
    visitor bytea NOT NULL,
    PRIMARY KEY (sandbox, day, visitor)
);

CREATE TABLE visitor_salts (
    day  date PRIMARY KEY,
    salt bytea NOT NULL
);

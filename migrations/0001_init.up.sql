-- pgcrypto supplies gen_random_uuid() for ad-hoc queries and fixtures only:
-- entity ids are application-generated UUIDv7, so one id can be reused across
-- every row of a single commit (A.1).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

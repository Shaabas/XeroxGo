-- XG-CLOUD-DOCS-V2: PRIVATE storage foundation (NOT applied until approved).
-- Private buckets; never expose student documents through public URLs.
insert into storage.buckets (id, name, public, file_size_limit)
values ('xeroxgo-originals', 'xeroxgo-originals', false, 26214400)
on conflict (id) do nothing;
insert into storage.buckets (id, name, public, file_size_limit)
values ('xeroxgo-prepared', 'xeroxgo-prepared', false, 52428800)
on conflict (id) do nothing;

create table if not exists public.document_uploads (
  upload_id text primary key,
  upload_token_hash text not null,
  kiosk_id text not null,
  original_name text not null,
  object_key text not null unique,
  content_type text not null,
  declared_bytes bigint not null check (declared_bytes > 0 and declared_bytes <= 26214400),
  verified_bytes bigint,
  verified_sha256 text,
  state text not null default 'PENDING_UPLOAD'
    check (state in ('PENDING_UPLOAD','UPLOADED','PREPARING','READY','FAILED','EXPIRED')),
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  verified_at timestamptz
);
create index if not exists document_uploads_expiry_idx
  on public.document_uploads (expires_at, state);

create table if not exists public.document_variants (
  variant_id text primary key,
  upload_id text not null references public.document_uploads(upload_id),
  settings_hash text not null,
  normalizer_version text not null,
  storage_key text unique,
  checksum_sha256 text,
  billable_sides integer,
  total_sheets integer,
  canonical_price_paise bigint,
  tariff_revision bigint,
  state text not null default 'PREPARING'
    check (state in ('PREPARING','READY','FAILED','EXPIRED')),
  error_code text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  unique (upload_id, settings_hash, normalizer_version)
);
create index if not exists document_variants_expiry_idx
  on public.document_variants (expires_at, state);

-- Order references must be immutable once checkout succeeds. Application
-- transaction must also ensure no PREPARING variant can be attached.
create table if not exists public.order_documents (
  order_id text not null references public.orders(order_id) on delete cascade,
  sequence_no integer not null check (sequence_no >= 1),
  variant_id text not null references public.document_variants(variant_id),
  accepted_price_paise bigint not null check (accepted_price_paise > 0),
  primary key (order_id, sequence_no)
);

-- No browser-side Data API access to these tables. Render service role only.
alter table public.document_uploads enable row level security;
alter table public.document_variants enable row level security;
alter table public.order_documents enable row level security;
revoke all on public.document_uploads, public.document_variants, public.order_documents
  from PUBLIC, anon, authenticated;

-- Reference-only checkout idempotency. Unique across all cloud checkout attempts.
-- The upload capability authorizes recovery of the corresponding order.
create table if not exists public.cloud_order_requests (
  request_id text primary key,
  upload_id text not null references public.document_uploads(upload_id),
  variant_id text not null references public.document_variants(variant_id),
  order_id text not null unique references public.orders(order_id) on delete cascade,
  kiosk_id text not null,
  accepted_price_paise bigint not null check (accepted_price_paise > 0),
  tariff_revision bigint not null check (tariff_revision > 0),
  created_at timestamptz not null default now()
);
alter table public.cloud_order_requests enable row level security;
revoke all on public.cloud_order_requests from PUBLIC, anon, authenticated;

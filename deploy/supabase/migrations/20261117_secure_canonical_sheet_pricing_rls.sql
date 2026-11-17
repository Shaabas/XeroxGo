-- Migration: 20261117_secure_canonical_sheet_pricing_rls.sql
-- Goal: Restrict public access and enable Row Level Security on canonical sheet pricing tables.
--
-- Security hardening applied:
-- 1. Revoke all privileges from PUBLIC, anon, and authenticated roles.
-- 2. Enable Row Level Security (RLS) on sheet_tariff_versions and sheet_tariff_state.

REVOKE ALL PRIVILEGES ON TABLE public.sheet_tariff_versions, public.sheet_tariff_state FROM PUBLIC, anon, authenticated;

ALTER TABLE public.sheet_tariff_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sheet_tariff_state ENABLE ROW LEVEL SECURITY;

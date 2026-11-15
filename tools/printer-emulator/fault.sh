#!/usr/bin/env bash
# fault.sh - CLI for XeroxGo Coordinated IPP + SNMP Fault Injection
#
# Delegates directly to emulator.sh fault command.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "${SCRIPT_DIR}/emulator.sh" fault "$@"

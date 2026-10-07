#!/usr/bin/env python3
"""Generate the lab's throwaway PKI for the TLS scenarios. Run by lab/record.py at record time.

    uv run --with cryptography==50.0.1 python lab/make_certs.py <out-dir>

Writes PEM files to <out-dir> (never to the repo, because private keys must not reach git):
  internal-ca.crt/.key   the CA the services are meant to trust
  legacy-ca.crt/.key     an unrelated, older CA (the wrong trust bundle in the truststore scenario)
  payments-api.crt/.key  valid 1 year, SAN payments-api.payments.svc[.cluster.local], signed by internal-ca
  auth-api.crt/.key      EXPIRED yesterday, SAN auth-api.identity.svc[.cluster.local], signed by internal-ca
"""
from __future__ import annotations

import datetime as dt
import sys
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID


def _key():
    return ec.generate_private_key(ec.SECP256R1())


def _name(cn: str, org: str) -> x509.Name:
    return x509.Name([x509.NameAttribute(NameOID.ORGANIZATION_NAME, org), x509.NameAttribute(NameOID.COMMON_NAME, cn)])


def ca(cn: str, org: str, now: dt.datetime):
    k = _key()
    c = (x509.CertificateBuilder().subject_name(_name(cn, org)).issuer_name(_name(cn, org)).public_key(k.public_key())
         .serial_number(x509.random_serial_number()).not_valid_before(now - dt.timedelta(days=1))
         .not_valid_after(now + dt.timedelta(days=3650))
         .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
         .sign(k, hashes.SHA256()))
    return k, c


def leaf(cn: str, sans: list[str], ca_key, ca_cert, not_before: dt.datetime, not_after: dt.datetime):
    k = _key()
    c = (x509.CertificateBuilder().subject_name(_name(cn, "Lab Services")).issuer_name(ca_cert.subject).public_key(k.public_key())
         .serial_number(x509.random_serial_number()).not_valid_before(not_before).not_valid_after(not_after)
         .add_extension(x509.SubjectAlternativeName([x509.DNSName(s) for s in sans]), critical=False)
         .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
         .sign(ca_key, hashes.SHA256()))
    return k, c


def write(out: Path, name: str, key, cert) -> None:
    (out / f"{name}.crt").write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    (out / f"{name}.key").write_bytes(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                                                         serialization.NoEncryption()))


def main() -> int:
    out = Path(sys.argv[1])
    out.mkdir(parents=True, exist_ok=True)
    now = dt.datetime.now(dt.UTC)
    ik, ic = ca("Internal Services CA 2026", "Platform", now)
    lk, lc = ca("Legacy Services CA 2021", "Platform", now - dt.timedelta(days=1500))
    write(out, "internal-ca", ik, ic)
    write(out, "legacy-ca", lk, lc)
    svc = lambda n, ns: [f"{n}.{ns}.svc", f"{n}.{ns}.svc.cluster.local", n]  # noqa: E731
    write(out, "payments-api", *leaf("payments-api", svc("payments-api", "payments"), ik, ic, now - dt.timedelta(days=1), now + dt.timedelta(days=365)))
    write(out, "auth-api", *leaf("auth-api", svc("auth-api", "identity"), ik, ic, now - dt.timedelta(days=91), now - dt.timedelta(days=1)))
    return 0


if __name__ == "__main__":
    sys.exit(main())

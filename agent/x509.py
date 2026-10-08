"""Read the few X.509 fields a diagnosis needs from a PEM certificate. Standard library only (D-31).

    info = parse_pem(pem_text)
    # {"subject_cn", "subject_o", "issuer_cn", "issuer_o", "not_before", "not_after", "dns_names", "is_ca"}

A minimal DER walker over the TBSCertificate: issuer, validity, subject, and the subjectAltName and
basicConstraints extensions. It does not verify signatures. It reports what a certificate claims, which
is what a diagnosis cites. Only public certificates ever reach it, because the doctor never reads Secrets.
"""
from __future__ import annotations

import base64
import datetime as dt
import re

_PEM = re.compile(r"-----BEGIN CERTIFICATE-----(.*?)-----END CERTIFICATE-----", re.S)
OID_CN, OID_O = bytes.fromhex("550403"), bytes.fromhex("55040a")
OID_SAN, OID_BC = bytes.fromhex("551d11"), bytes.fromhex("551d13")


def _tlv(b: bytes, i: int) -> tuple[int, int, int]:
    """(tag, content_start, content_end) of the element at i."""
    tag, n = b[i], b[i + 1]
    i += 2
    if n & 0x80:
        k = n & 0x7F
        n = int.from_bytes(b[i:i + k], "big")
        i += k
    return tag, i, i + n


def _children(b: bytes, start: int, end: int) -> list[tuple[int, int, int]]:
    out, i = [], start
    while i < end:
        t, s, e = _tlv(b, i)
        out.append((t, s, e))
        i = e
    return out


def _name(b: bytes, s: int, e: int) -> dict[str, str]:
    out = {}
    for _, rs, re_ in _children(b, s, e):                  # SET OF
        for _, as_, ae in _children(b, rs, re_):           # AttributeTypeAndValue SEQUENCE
            (_, os_, oe), (_, vs, ve) = _children(b, as_, ae)[:2]
            oid = b[os_:oe]
            if oid in (OID_CN, OID_O):
                out["cn" if oid == OID_CN else "o"] = b[vs:ve].decode("utf-8", "replace")
    return out


def _time(b: bytes, t: int, s: int, e: int) -> str:
    raw = b[s:e].decode()
    fmt = "%y%m%d%H%M%SZ" if t == 0x17 else "%Y%m%d%H%M%SZ"
    return dt.datetime.strptime(raw, fmt).replace(tzinfo=dt.UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_der(der: bytes) -> dict:
    _, cs, ce = _tlv(der, 0)
    _, ts, te = _children(der, cs, ce)[0]                  # TBSCertificate
    kids = _children(der, ts, te)
    if kids[0][0] == 0xA0:                                 # [0] version present
        kids = kids[1:]
    issuer, validity, subject = kids[2], kids[3], kids[4]
    (nbt, nbs, nbe), (nat, nas, nae) = _children(der, validity[1], validity[2])
    info = {"issuer": _name(der, issuer[1], issuer[2]), "subject": _name(der, subject[1], subject[2]),
            "not_before": _time(der, nbt, nbs, nbe), "not_after": _time(der, nat, nas, nae), "dns_names": [], "is_ca": False}
    for t, s, e in kids[5:]:
        if t != 0xA3:
            continue
        for _, xs, xe in _children(der, *_children(der, s, e)[0][1:]):      # Extensions SEQUENCE → Extension
            parts = _children(der, xs, xe)
            oid = der[parts[0][1]:parts[0][2]]
            _, vs, ve = parts[-1]                                           # OCTET STRING extnValue
            if oid == OID_SAN:
                _, ss, se = _tlv(der, vs)
                info["dns_names"] = [der[a:z].decode() for tag, a, z in _children(der, ss, se) if tag == 0x82]
            elif oid == OID_BC:
                _, bs, be = _tlv(der, vs)
                info["is_ca"] = any(tag == 0x01 and der[a:z] != b"\x00" for tag, a, z in _children(der, bs, be))
    return {"subject_cn": info["subject"].get("cn"), "subject_o": info["subject"].get("o"),
            "issuer_cn": info["issuer"].get("cn"), "issuer_o": info["issuer"].get("o"),
            "not_before": info["not_before"], "not_after": info["not_after"],
            "dns_names": info["dns_names"], "is_ca": info["is_ca"]}


def parse_pem(text: str) -> list[dict]:
    """Every certificate in a PEM bundle, in order."""
    return [parse_der(base64.b64decode("".join(m.group(1).split()))) for m in _PEM.finditer(text)]

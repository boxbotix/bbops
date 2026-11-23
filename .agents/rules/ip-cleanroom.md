---
trigger: always_on
---

# Zero-GPLv3 & Pure `common.xml` Clean-Room Enforcement

Never read, search, fetch, import, or translate code or schemas from GPLv3 repositories (`ArduPilot/ardupilot`, `betaflight/betaflight`, `iNavFlight/inav`, `cleanflight/cleanflight`) or `ardupilotmega.xml`, and never import retired `semlink` mesh or legacy `semops` packages. Only Tier-1 permissive sources listed in `PROVENANCE.md` (`C360Studio/semengine`, `C360Studio/semboids`, `C360Studio/ateam`, `boxbotix/bbstack`, MIT `mavlink/mavlink` `minimal.xml` + `common.xml`, and MIT Go MAVLink v2 framing restricted to `common` dialect) and published academic literature may be referenced, preserving upstream SPDX copyright and 40-char commit SHA headers on adapted files.

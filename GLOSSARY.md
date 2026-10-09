# ArchiveBridge

ArchiveBridge preserves the files and relationships present in a selected personal data export.

## Language

**Source archive**: One selected export part. Together, the selected parts define the scope of a transfer.
_Avoid_: Account backup, complete library

**Media occurrence**: A photo or video appearing at a particular location in a source archive. Identical content can have several occurrences with different relationships.
_Avoid_: Unique photo

**Sidecar**: A separate metadata file accompanying a media occurrence or album.
_Avoid_: Embedded metadata

**Metadata match**: An association between a sidecar and a media occurrence supported by an unambiguous source relationship.
_Avoid_: Best guess

**Album relationship**: A media occurrence's membership in a source album. One media item can belong to several albums.
_Avoid_: Storage folder

**Portable archive**: The transferred originals, preserved sidecars and a manifest describing the selected source scope and preserved relationships.
_Avoid_: Full account recovery

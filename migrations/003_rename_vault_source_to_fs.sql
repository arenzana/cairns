-- Rename the "vault" source to the generic "fs".
--
-- The first source was written as an Obsidian vault reader and named for it.
-- That leaked an app-specific assumption into the schema: the default source
-- indexes a directory of markdown, and Obsidian is one optional behaviour on
-- top (obsidian:// links, wikilink extraction), not the thing itself.
--
-- Pure rename. No embedding changes, so no re-index is needed: the vectors and
-- chunks are untouched and only the locator prefix and discriminator move.
UPDATE documents
   SET source = 'fs',
       uri    = 'fs:' || substring(uri from length('vault:') + 1)
 WHERE source = 'vault';

UPDATE source_state SET source = 'fs' WHERE source = 'vault';
UPDATE sweep_progress SET source = 'fs' WHERE source = 'vault';

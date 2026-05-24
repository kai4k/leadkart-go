<!--
Placeholder negative-fixture entry for TestArch_TestArch_NoTODOWithoutTicket.

This directory is a catalog stub — its existence satisfies
TestMeta_EveryFitnessFunctionHasNegativeFixture per
Ford / Parsons / Kua *Building Evolutionary Architectures* ch.4
(every fitness function must be shown to FAIL against a known-bad
sample at least once before we trust it).

To fill in the actual fixture:
  1. Add Go source (or YAML/SQL, depending on what the test scans)
     to this directory that deliberately violates the rule the
     parent test enforces.
  2. Extend the fitness-function runner to point at this directory
     in a SUBTEST + assert the test FAILS.
  3. The placeholder file may then be deleted.

Until then, the directory's presence is the load-bearing marker.
-->

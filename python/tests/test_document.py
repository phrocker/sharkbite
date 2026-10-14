# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements. See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership. The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License. You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied. See the License for the
# specific language governing permissions and limitations
# under the License.

import unittest
from dataclasses import FrozenInstanceError

from shoal import ErrorCode, ShoalError, is_error_code
from shoal.document import Citation, Document, SourcePosition, SourceRange


class DocumentTest(unittest.TestCase):
    def test_citation_requires_revision_specific_source(self) -> None:
        citation = Citation(
            document_id="doc-1",
            revision_id="rev-2",
            section_id="section-3",
            range=SourceRange(
                start=SourcePosition(offset=12, page=2),
                end=SourcePosition(offset=24, page=2),
            ),
        )
        citation.validate()

        with self.assertRaises(ShoalError) as raised:
            Citation(
                document_id="doc-1",
                section_id="section-3",
            ).validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_source_range_rejects_reversed_coordinates(self) -> None:
        source_range = SourceRange(
            start=SourcePosition(offset=20, page=3),
            end=SourcePosition(offset=10, page=2),
        )
        with self.assertRaises(ShoalError) as raised:
            source_range.validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_source_range_matches_half_open_and_unknown_page_semantics(
        self,
    ) -> None:
        SourceRange(
            start=SourcePosition(offset=12, page=3),
            end=SourcePosition(offset=12, page=3),
        ).validate()
        SourceRange(
            start=SourcePosition(offset=12, page=3),
            end=SourcePosition(offset=24, page=0),
        ).validate()

        for source_range in (
            SourceRange(
                start=SourcePosition(offset=-1),
                end=SourcePosition(offset=0),
            ),
            SourceRange(
                start=SourcePosition(offset=0, page=-1),
                end=SourcePosition(offset=0),
            ),
            SourceRange(
                start=SourcePosition(offset=0, page=3),
                end=SourcePosition(offset=0, page=2),
            ),
        ):
            with self.subTest(source_range=source_range):
                with self.assertRaises(ShoalError) as raised:
                    source_range.validate()
                self.assertTrue(
                    is_error_code(
                        raised.exception,
                        ErrorCode.INVALID_ARGUMENT,
                    )
                )

    def test_source_range_rejects_non_wire_coordinates(self) -> None:
        invalid_ranges = (
            SourceRange(
                start=SourcePosition(offset=True),
                end=SourcePosition(offset=1),
            ),
            SourceRange(
                start=SourcePosition(offset=1.5),  # type: ignore[arg-type]
                end=SourcePosition(offset=2),
            ),
            SourceRange(
                start=SourcePosition(offset=0),
                end=SourcePosition(offset=1 << 63),
            ),
            SourceRange(
                start=SourcePosition(offset=0, page=1 << 31),
                end=SourcePosition(offset=1, page=1 << 31),
            ),
        )
        for source_range in invalid_ranges:
            with self.subTest(source_range=source_range):
                with self.assertRaises(ShoalError):
                    source_range.validate()

    def test_citation_range_error_preserves_wrapped_error_semantics(
        self,
    ) -> None:
        citation = Citation(
            document_id="doc-1",
            revision_id="rev-1",
            section_id="section-1",
            range=SourceRange(
                start=SourcePosition(offset=2),
                end=SourcePosition(offset=1),
            ),
        )

        with self.assertRaisesRegex(
            RuntimeError,
            "^citation: invalid_argument: invalid source offsets$",
        ) as raised:
            citation.validate()
        self.assertTrue(
            is_error_code(raised.exception, ErrorCode.INVALID_ARGUMENT)
        )

    def test_document_is_frozen_with_metadata_snapshot(self) -> None:
        metadata = {"source": "wiki"}
        document = Document(id="doc-1", metadata=metadata)
        metadata["source"] = "changed"

        self.assertEqual(document.metadata["source"], "wiki")
        with self.assertRaises(TypeError):
            document.metadata["source"] = "changed"  # type: ignore[index]
        with self.assertRaises(FrozenInstanceError):
            document.title = "changed"  # type: ignore[misc]


if __name__ == "__main__":
    unittest.main()

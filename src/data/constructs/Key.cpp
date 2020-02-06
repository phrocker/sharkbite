/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "data/constructs/Key.h"

namespace cclient
{
namespace data
{

Key::Key (const char * const userRow) :
    deleted (false), timestamp (9223372036854775807L), colVisSize (0), rowMaxSize (
        0), columnFamilySize (0), colQualSize (0), rowLength (0), columnFamilyLength (
            0), colQualLen (0)
{
	if (userRow)
		setRow(userRow,strlen(userRow));
	else
		row = new char[0];

    colFamily = new char[0];

    colQualifier = new char[0];

    keyVisibility = new char[0];

}

Key::~Key ()
{

    delete[] row;
    delete[] colFamily;
    delete[] colQualifier;
    delete[] keyVisibility;

}

void
Key::setRow (const char *r, uint32_t size)
{
    if (size > rowMaxSize)
    {
        delete[] row;
        row = new char[size];
        rowMaxSize = size;
    }

    memcpy (row, r, size);
    rowLength = size;

}

void
Key::setColFamily (const char *r, uint32_t size)
{

    if (size > columnFamilySize)
    {
        delete[] colFamily;
        colFamily = new char[size];
        columnFamilySize = size;
    }

    memcpy (colFamily, r, size);
    columnFamilyLength = size;

}

void
Key::setColQualifier (const char *r, uint32_t size, uint32_t offset)
{
    if (offset + size > colQualSize)
    {
        char *nr = new char[size + offset];
        memcpy (nr, colQualifier, colQualSize);
        delete[] colQualifier;
        colQualifier = nr;
        colQualSize = size + offset;
    }

    memcpy (colQualifier + offset, r, size);
    colQualLen = size + offset;

}

void
Key::setColVisibility (const char *r, uint32_t size)
{
    if (size > colVisSize)
    {
        delete[] keyVisibility;
        keyVisibility = new char[size];
        colVisSize = size;
    }

    memcpy (keyVisibility, r, size);

}

bool
Key::operator < (const Key &rhs) const
{
    int compare = compareBytes (row, 0, rowLength, rhs.row, 0, rhs.rowLength);


    if (compare < 0)
        return true;
    else if (compare > 0)
        return false;
    compare = compareBytes (colFamily, 0, columnFamilyLength, rhs.colFamily,
                            0, rhs.columnFamilyLength);

    if (compare < 0)
        return true;
    else if (compare > 0)
        return false;
    compare = compareBytes (colQualifier, 0, colQualLen, rhs.colQualifier, 0,
                            rhs.colQualLen);

    if (compare < 0)
        return true;

    return false;
}

bool
Key::operator == (const Key & rhs) const
{
    int compare = compareBytes (row, 0, rowLength, rhs.row, 0,
                                rhs.columnFamilyLength);

    if (compare != 0)
        return false;

    compare = compareBytes (colFamily, 0, columnFamilyLength, rhs.colFamily,
                            0, rhs.columnFamilyLength);


    if (compare != 0)
        return false;

    compare = compareBytes (colQualifier, 0, colQualLen, rhs.colQualifier, 0,
                            rhs.colQualLen);


    if (compare != 0)
        return false;

    return (timestamp < rhs.timestamp);

}

uint64_t
Key::write (cclient::data::streams::OutputStream *outStream)
{

    uint32_t offset = rowLength;

    outStream->writeHadoopLong (offset);
    //outStream->writeHadoopLong( offset ); // cf offset

    offset += columnFamilyLength;
    outStream->writeHadoopLong (offset);
    //outStream->writeHadoopLong( offset ); // cq offset

    offset += colQualLen;
    outStream->writeHadoopLong (offset);
    //outStream->writeHadoopLong( offset ); // colvis offset

    offset += colVisSize;
    outStream->writeHadoopLong (offset);
    //outStream->writeHadoopLong( offset ); // total

    outStream->writeBytes (row, rowLength);
    outStream->writeBytes (colFamily, columnFamilyLength);
    outStream->writeBytes (colQualifier, colQualLen);
    outStream->writeBytes (keyVisibility, colVisSize);
    outStream->writeHadoopLong (timestamp);
    //outStream->writeHadoopLong( timestamp);

    return outStream->writeBoolean (deleted);

}

uint64_t
Key::read (cclient::data::streams::InputStream *in)
{
    int colFamilyOffset = in->readEncodedLong ();
    int colQualifierOffset = in->readEncodedLong ();
    int colVisibilityOffset = in->readEncodedLong ();
    int totalLen = in->readEncodedLong ();

    row = new char[colFamilyOffset];
    in->readBytes (row, colFamilyOffset);

    colFamily = new char[colQualifierOffset - colFamilyOffset];
    in->readBytes (colFamily, colQualifierOffset - colFamilyOffset);
    columnFamilySize = colQualifierOffset - colFamilyOffset;

    colQualifier = new char[colVisibilityOffset - colQualifierOffset];
    in->readBytes (colQualifier, colVisibilityOffset - colQualifierOffset);
    colQualSize = colVisibilityOffset - colQualifierOffset;

    keyVisibility = new char[totalLen - colVisibilityOffset];
    in->readBytes (keyVisibility, totalLen - colVisibilityOffset);
    colVisSize = totalLen - colVisibilityOffset;

    timestamp = in->readEncodedLong ();

    deleted = in->readBoolean ();

    return in->getPos();
}

} /* namespace data */
} /* namespace cclient */

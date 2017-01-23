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
#include <string>


#include "../../../include/data/constructs/inputvalidation.h"

#include "../../../include/data/constructs/column.h"

namespace cclient
{
namespace data
{

Column::Column (std::string *columFam, std::string *columnQual, std::string *columnVis)
{
    if (!IsEmpty (columFam))
    {
        columnFamily = new char[columFam->size () + 1];
        memcpy (columnFamily, columFam->c_str (), columFam->size ());
        columnFamilyLen = columFam->size ();
    }
    else
    {
        columnFamily = new char[0];
        columnFamilyLen = 0;
    }

    if (!IsEmpty (columnQual))
    {
        columnQualifier = new char[columnQual->size () + 1];
        memcpy (columnQualifier, columnQual->c_str (), columnQual->size ());
        columnQualifierLen = columnQual->size ();
    }
    else
    {
        columnQualifier = new char[0];
        columnQualifierLen = 0;
    }

    if (!IsEmpty (columnVis))
    {
        columnVisibility = new char[columnVis->size () + 1];
        memcpy (columnVisibility, columnVis->c_str (), columnVis->size ());
        columnVisibilityLen = columnVis->size ();
    }
    else
    {
        columnVisibility = new char[0];
        columnVisibilityLen = 0;
    }
}

void
Column::setColFamily (const char *r, uint32_t size)
{

    if (columnFamilyLen > 0)
    {
        delete[] columnFamily;
        columnFamily = new char[size];
        columnFamilyLen = size;
    }

    memcpy (columnFamily, r, size);
    columnFamilyLen = size;

}

void
Column::setColQualifier (const char *r, uint32_t size)
{
    if (columnQualifierLen > 0)
    {
        delete[] columnQualifier;
        columnQualifier = new char[size];
        columnQualifierLen = size;
    }

    memcpy (columnQualifier, r, size);
    columnQualifierLen = size;

}

void
Column::setColVisibility (const char *r, uint32_t size)
{
    if (columnVisibilityLen > 0)
    {
        delete[] columnVisibility;
        columnVisibility = new char[size];
        columnVisibilityLen = size;
    }

    memcpy (columnVisibility, r, size);
    columnVisibilityLen = size;

}

Column::~Column ()
{
    delete[] columnFamily;
    delete[] columnQualifier;
    delete[] columnVisibility;
}

Column&
Column::operator= (const Column& other)
{
    setColFamily(other.columnFamily,other.columnFamilyLen);
    setColQualifier(other.columnQualifier,other.columnQualifierLen);
    setColVisibility(other.columnVisibility,other.columnVisibilityLen);
    return *this;
}

bool
Column::operator < (const Column &rhs) const
{

    int compare = compareBytes (columnFamily, 0, columnFamilyLen,
                                rhs.columnFamily, 0, rhs.columnFamilyLen);

    if (compare < 0)
        return true;
    else if (compare > 0)
        return false;;

    compare = compareBytes (columnQualifier, 0, columnQualifierLen,
                            rhs.columnQualifier, 0, rhs.columnQualifierLen);

    if (compare < 0)
        return true;
    else if (compare > 0)
        return false;

    compare = compareBytes (columnVisibility, 0, columnVisibilityLen,
                            rhs.columnVisibility, 0, rhs.columnVisibilityLen);

    if (compare < 0)
        return true;

    return false;

}

bool
Column::operator == (const Column & rhs) const
{
    int compare = compareBytes (columnFamily, 0, columnFamilyLen,
                                rhs.columnFamily, 0, rhs.columnFamilyLen);

    if (compare != 0)
        return false;

    compare = compareBytes (columnQualifier, 0, columnQualifierLen,
                            rhs.columnQualifier, 0, rhs.columnQualifierLen);

    if (compare != 0)
        return false;

    compare = compareBytes (columnVisibility, 0, columnVisibilityLen,
                            rhs.columnVisibility, 0, rhs.columnVisibilityLen);

    if (compare != 0)
        return false;
    else
        return true;

}

uint64_t
Column::write (cclient::data::streams::OutputStream *outStream)
{

    if (IsEmpty (columnFamily))
    {
        outStream->writeBoolean (false);
    }
    else
    {
        outStream->writeBoolean (true);
        outStream->writeBytes (columnFamily, columnFamilyLen);
    }

    if (IsEmpty (columnQualifier))
    {
        outStream->writeBoolean (false);
    }
    else
    {
        outStream->writeBoolean (true);
        outStream->writeBytes (columnQualifier, columnQualifierLen);
    }

    if (IsEmpty (columnVisibility))
    {
        return outStream->writeBoolean (false);
    }
    else
    {
        outStream->writeBoolean (true);
        return outStream->writeBytes (columnVisibility, columnVisibilityLen);
    }

}

}
}

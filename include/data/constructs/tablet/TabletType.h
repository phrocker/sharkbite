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
#ifndef TABLETTYPE_H_
#define TABLETTYPE_H_

#include "../KeyExtent.h"
#include "../inputvalidation.h"


namespace cclient
{
namespace data
{
namespace tserver
{

enum TabletType
{
    ROOT, METADATA, USER
};

static TabletType fromExtent(KeyExtent *extent)
{
    if (!IsEmpty(extent))
    {

        if (*extent == ROOT_TABLET_EXTENT)
        {
            return ROOT;
        }
        if (extent->getTableId() == METADATA_TABLE_ID)
        {
            return METADATA;
        }
    }
    return USER;
}

}
}
}

#endif /* TABLETTYPE_H_ */

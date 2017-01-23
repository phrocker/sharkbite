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

#ifndef METABLOCK_H_
#define METABLOCK_H_

// magic number for RFile.
#define MAGIC_NUMBER 0x20637474
#define MAGIC_NUMBER2 0x74746320
// rfile version.
#define RFILE_VERSION 4
#define RFILE_VERSION_3 3
#define RFILE_VERSION_6 6
#define RFILE_VERSION_7 7



#include <map>

#include <vector>
#include <stdexcept>



#include "../../../streaming/DataOutputStream.h"
#include "../../../streaming/Streams.h"

#include "LocalityGroupMetaData.h"

namespace cclient {
namespace data {

class MetaBlock: public cclient::data::streams::StreamInterface {
public:
    /**
     Default constructor.
     **/
    MetaBlock();

    ~MetaBlock();

    /**
     Adds a list of locality groups to the RFile Meta Block
     **/
    void addLocalityGroups(std::vector<LocalityGroupMetaData*> lgs) {
        localityGroups.insert(localityGroups.end(), lgs.begin(), lgs.end());
    }

    uint64_t write(cclient::data::streams::DataOutputStream *outStream);
protected:
    std::vector<LocalityGroupMetaData*> localityGroups;
};

}
}
#endif /* METABLOCK_H_ */

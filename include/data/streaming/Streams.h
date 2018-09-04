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
#ifndef DATA_WRITER
#define DATA_WRITER

#include <memory>
#include <string>

#include <cstring>


#include <stdint.h>
#include <netinet/in.h>

#include "input/InputStream.h"

#include "OutputStream.h"
#include "DataOutputStream.h"
#include "HdfsOutputStream.h"

namespace cclient {
namespace data {
namespace streams {

class StreamInterface {
public:

    StreamInterface();
    virtual ~StreamInterface();

    virtual std::shared_ptr<StreamInterface> getStream();

    virtual uint64_t write (cclient::data::streams::OutputStream *out);
    virtual uint64_t read (cclient::data::streams::InputStream *in);

    virtual DataOutputStream *createDataStream(
        DataOutputStream *out,
        std::string name = "") {
        return NULL;
    }

};
}
}
}

#endif


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
#include <stdint.h>
#include <stdexcept>
#include <cstdio>
#include <cstdlib>
#include <vector>
#include <cstring>



#include "data/constructs/compressor/compressor.h"


void
cclient::data::compression::Compressor::setInput (const char *b, uint32_t offset, uint32_t length)
{
//  std::cout << "buffer address is " << buffer << std::endl;
    // grow the buffer, but do not remove it. 
    if (buffer != nullptr && length > size){
        delete[] buffer;
        buffer=nullptr;
        size = length;
    }else{
        buffer = new char[length];
    }
    memcpy (buffer, b, length);
    off = offset;
    len = length;
}

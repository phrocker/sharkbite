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
#include <iostream>

#include <stdexcept>
#include <cstring>

using namespace std;

#include <assert.h>

#include "../../../../../include/data/constructs/compressor/test/../compressor.h"
#include "../../../../../include/data/constructs/compressor/test/../zlibCompressor.h"
#include <streaming/ByteOutputStream.h>

using namespace cclient::data::compression;
using namespace cclient::data::streams;
int main() {
	Compressor *compress = new ZLibCompressor(64 * 1024 * 1024);
	char test[4] = { 0xde, 0xad, 0xbe, 0xef };
	compress->setInput(test, 0, 4);
	ByteOutputStream outStream(1024);
	compress->compress(&outStream);

	char *compressed = outStream.getByteArray();
	uint32_t size = outStream.getPos();

	compress->setInput(compressed, 0, size);

	ByteOutputStream decStream(1024);
	compress->decompress(&decStream);

	assert( memcmp(decStream.getByteArray(),test,4) == 0);

	delete compress;
	compress = new ZLibCompressor(64 * 1024 * 1024);
	outStream.flush();
	compress->compress(&outStream);

	assert( outStream.getPos() == 0);

	cout << "Successfully passed" << endl;

}

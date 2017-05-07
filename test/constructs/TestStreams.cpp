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
using namespace std;
#include "../../include/data/streaming/ByteOutputStream.h"
#include "../../include/data/streaming/NetworkOrderStream.h"
#include "../../include/data/streaming/input/ByteInputStream.h"
#include "../../include/data/streaming/input/NetworkOrderInputStream.h"


#define CATCH_CONFIG_MAIN

#include "../catch.hpp"
using namespace cclient::data;
using namespace cclient::data::streams;



TEST_CASE("TestByteStream", "[testSerDer]") {
  
  
	ByteOutputStream *endian = new ByteOutputStream(250 * 1024 * 1024);
	
	endian->writeInt(5);
	endian->writeString("Hello world");
	
	ByteInputStream *inVerification = new ByteInputStream(endian->getByteArray(),endian->getSize());
	
	REQUIRE(5 == inVerification->readInt());
	REQUIRE("Hello world" == inVerification->readString());
	
	
	
	delete inVerification;
	
	
	
	delete endian;
	
}


TEST_CASE("TestEndianStream", "[testSerDer]") {
  
	ByteOutputStream *byte = new ByteOutputStream(250 * 1024 * 1024);
	BigEndianOutStream *endian = new BigEndianOutStream(byte);
	
	endian->writeInt(5);
	endian->writeString("Hello world");
	
	EndianInputStream *inVerification = new EndianInputStream(byte->getByteArray(),byte->getSize());
	
	REQUIRE(5 == inVerification->readInt());
	REQUIRE("Hello world" == inVerification->readString());
	
	
	
	delete inVerification;
	
	
	
	delete endian;
	
	delete byte;
	
}

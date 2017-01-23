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

#include "../../../include/data/constructs/KeyValue.h"

namespace cclient
{
namespace data
{

KeyValue::KeyValue () :
	key (new Key ()), value (new Value()), my_alloc (true)
{

}

KeyValue::~KeyValue ()
{
	if (my_alloc)
	{
		delete key;
	}
	if (NULL != value)
	{
		delete value;
	}
}

void
KeyValue::setKey (Key *k, bool set_ownership)
{
	if (my_alloc) {
		delete key;
		my_alloc = set_ownership;
	}
	key = k;
}

Key *
KeyValue::getKey ()
{
	return key;
}

cclient::data::streams::StreamInterface *
KeyValue::getStream ()
{
	return key->getStream ();
}

Value *
KeyValue::getValue ()
{
	return value;
}

void
KeyValue::setValue (Value *v)
{
	if (NULL != value) {
		delete value;

	}
	value = new Value ();
	value->setValue (v->getValue ().first, v->getValue ().second);
}

void
KeyValue::setValue (uint8_t *b, size_t size)
{
	if (NULL == value) {
		value = new Value ();
	}
	value->setValue (b, size);
}

uint64_t
KeyValue::write (cclient::data::streams::OutputStream *outStream)
{

	key->write (outStream);

	return value->write (outStream);
}

KeyValue &
KeyValue::operator= (const KeyValue &other)
{

	key = other.key;

	std::pair<uint8_t*, uint32_t> p = other.value->getValue ();

	value->setValue (p.first, p.second);
	return *this;
}

bool
KeyValue::operator < (const KeyValue &rhs) const
{
	return (*(Key*) key->getStream () < *(Key*) (rhs.key->getStream ()));
}

bool
KeyValue::operator < (const KeyValue *rhs) const
{
	return *this < *rhs;

}

bool
KeyValue::operator == (const KeyValue & rhs) const
{
	return ((Key*) key->getStream () == (Key*) (rhs.key->getStream ()))
	       && (value == rhs.value);
}
bool
KeyValue::operator != (const KeyValue &rhs) const
{
	return !(*this == rhs);
}

bool
KeyValue::operator == (const KeyValue *rhs) const
{
	return *this == *rhs;
}
bool
KeyValue::operator != (const KeyValue *rhs) const
{
	return !(*this == *rhs);
}

KeyValue& KeyValue::operator=(KeyValue&& other)
{
    key = other.key;
    value = other.value;
    my_alloc = other.my_alloc;
    return *this;
}


} /* namespace data */
} /* namespace cclient */

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

#ifndef KEYVALUE_H_
#define KEYVALUE_H_

#include <stdint.h>
#include "Key.h"
#include "value.h"
#include "rkey.h"
#include "../streaming/Streams.h"




namespace cclient
{
namespace data
{
  /**
 * Stores Keys and values within a stream object.
 **/
class KeyValue : public cclient::data::streams::StreamInterface
{
public:
	/**
	 * Key value constructor.
	 **/
	KeyValue();
	
	/**
	 * Key value constructor.
	 **/
	explicit KeyValue(KeyValue&& other){
	  key = other.key;
	  value = other.value;
	  my_alloc = other.my_alloc;
	}


	virtual ~KeyValue();

	/**
	 * Sets the key within this object.
	 **/
	void setKey(Key *k, bool set_ownership = false);

	Key *getKey();
	
	inline bool hasOwnerShip()
	{
	  return my_alloc;
	}

	StreamInterface *getStream();

	Value *getValue();
	void setValue(Value *v);
	
	 friend inline std::ostream &
	operator << (std::ostream &out, KeyValue &rhs)
	{
	    out << "key is " << rhs.key;
	    return out;
	}

	friend inline std::ostream &
	operator << (std::ostream &out, KeyValue *rhs)
	{
	    return operator<< (out, *rhs);
	}

	void setValue(uint8_t *b, size_t size);
	uint64_t write (cclient::data::streams::OutputStream *outStream);
	KeyValue &operator=(const KeyValue &other);
	bool operator <(const KeyValue &rhs) const;
	bool operator <(const KeyValue *rhs) const;
	KeyValue &operator=(KeyValue&& other);
	bool operator == (const KeyValue *rhs) const;
	bool operator == (const KeyValue & rhs) const;
	bool operator != (const KeyValue &rhs) const;
	bool operator != (const KeyValue *rhs) const;




protected:
	Key *key;
	Value *value;
	bool my_alloc;

};

} /* namespace data */
} /* namespace cclient */
#endif /* KEYVALUE_H_ */

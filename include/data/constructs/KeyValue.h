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
#include <memory>
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
	
	virtual ~KeyValue();

	explicit KeyValue(const std::shared_ptr<Key> &, const std::shared_ptr<Value> &);

	/**
	 * Sets the key within this object.
	 **/
	void setKey(std::shared_ptr<Key> k, bool set_ownership = false);

	std::shared_ptr<Key> getKey();
	
	inline bool hasOwnerShip()
	{
	  return false;
	}

	std::shared_ptr<StreamInterface> getStream() override;

	std::shared_ptr<Value> getValue();
	void setValue(const std::shared_ptr<Value> &v);
	
	 friend inline std::ostream &
	operator << (std::ostream &out, const KeyValue &rhs)
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
	uint64_t write (cclient::data::streams::OutputStream *outStream) override;
	KeyValue &operator=(const KeyValue &other);
	bool operator <(const KeyValue &rhs) const;
	bool operator <(const KeyValue *rhs) const;
	bool operator == (const KeyValue *rhs) const;
	bool operator == (const KeyValue & rhs) const;
	bool operator != (const KeyValue &rhs) const;
	bool operator != (const KeyValue *rhs) const;




protected:
	std::shared_ptr<Key> key;
	std::shared_ptr<Value> value;

};

} /* namespace data */
} /* namespace cclient */
#endif /* KEYVALUE_H_ */

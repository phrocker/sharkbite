CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: thrift

clean: 
	$(RM) $(OBJS)

# sed -i s/apache::thrift::to_string/::apache::thrift::to_string/ *.cpp
thrift:	$(OBJS)
	g++ -o thrift.so $(OBJS) $(LDFLAGS) -shared
	$(RM) $(OBJS)

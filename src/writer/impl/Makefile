CPPFLAGS=-g -std=c++11 -I/usr/local/include/thrift -lthrift -lboost_system -lpthread -lboost_thread   -DHAVE_CONFIG_H  -fPIC
LDFLAGS=
LDLIBS=
RM=rm -f
PWD=`pwd`


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: writer

clean: 
	$(RM) $(OBJS)


writer:	$(OBJS)
	g++ -o writer.so $(OBJS) $(LDFLAGS) $(CPPFLAGS) -shared -fPIC
	$(RM) $(OBJS)

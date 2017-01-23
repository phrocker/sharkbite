CPPFLAGS=-g -std=c++11 -I/usr/local/include/thrift -lthrift -lboost_system -lpthread -lboost_thread -fpermissive -DHAVE_CONFIG_H -fPIC
LDFLAGS=-g 
LDLIBS=
RM=rm -f
PWD=`pwd`


#SRCS=InterConnect.cpp TabletServer.cpp
SRCS=impl/Scanner.cpp
#
OBJS=$(subst .cpp,.o,$(SRCS))


all: scanner

clean: 
	$(RM) $(OBJS)


scanner:	$(OBJS)
	g++ -o scanner.so $(OBJS) -shared -fPIC
	
